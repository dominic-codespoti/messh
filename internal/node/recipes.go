package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"messh/internal/recipes"
)

// registerRecipeAPI exposes owner-only recipe CRUD on the control-token API.
func (n *Node) registerRecipeAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/recipes", n.apiRecipeList)
	api.HandleFunc("POST /v1/recipes", n.apiRecipePublish)
	api.HandleFunc("GET /v1/recipes/{name}/{version}", n.apiRecipeGet)
	api.HandleFunc("POST /v1/recipes/{name}/{version}/disable", n.apiRecipeDisable)
}

func (n *Node) recipeRegistry() *recipes.Registry {
	if n.jobs == nil {
		return nil
	}
	return n.jobs.RecipeRegistry
}

func (n *Node) apiRecipeList(w http.ResponseWriter, r *http.Request) {
	reg := n.recipeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "recipe registry unavailable")
		return
	}
	include := r.URL.Query().Get("include_disabled")
	if include != "" && include != "true" && include != "false" {
		writeError(w, http.StatusBadRequest, "include_disabled must be true or false")
		return
	}
	writeJSON(w, http.StatusOK, reg.List(include == "true"))
}

func (n *Node) apiRecipePublish(w http.ResponseWriter, r *http.Request) {
	reg := n.recipeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "recipe registry unavailable")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recipe body")
		return
	}
	var d recipes.Definition
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid recipe body: "+err.Error())
		return
	}
	if err = ensureRecipeEOF(dec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid recipe body: "+err.Error())
		return
	}
	got, err := reg.Publish(d)
	if err != nil {
		status := http.StatusBadRequest
		if recipeErrorCode(err) == "recipe_version_conflict" {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (n *Node) apiRecipeGet(w http.ResponseWriter, r *http.Request) {
	reg := n.recipeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "recipe registry unavailable")
		return
	}
	got, err := reg.Get(r.PathValue("name"), r.PathValue("version"), r.URL.Query().Get("digest"))
	if err != nil {
		if errors.Is(err, recipes.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (n *Node) apiRecipeDisable(w http.ResponseWriter, r *http.Request) {
	reg := n.recipeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "recipe registry unavailable")
		return
	}
	got, err := reg.Disable(r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		if errors.Is(err, recipes.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func ensureRecipeEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func recipeErrorCode(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}

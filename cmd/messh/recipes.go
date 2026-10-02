package main

import (
 "bytes"
 "context"
 "encoding/json"
 "flag"
 "fmt"
 "io"
 "net/http"
 "net/url"
 "os"
 "text/tabwriter"

 "messh/internal/recipes"
)

func init(){
 add(&Command{Name:"recipe list",Summary:"list owner-published job recipes",Output:"[{definition, digest, disabled}]",Examples:[]string{"messh recipe list"},Flags:func(fs *flag.FlagSet){fs.Bool("all",false,"include disabled versions")},Run:func(c *Context)error{
  cl,_,err:=c.Node();if err!=nil{return err};path:="/v1/recipes";if c.Bool("all"){path+="?include_disabled=true"};var list []recipes.Recipe;if err=cl.Do(context.Background(),http.MethodGet,path,nil,&list);err!=nil{return err};return c.Emit(list,func(w io.Writer){tw:=tabwriter.NewWriter(w,0,4,2,' ',0);fmt.Fprintln(tw,"NAME\tVERSION\tDIGEST\tSTATE\tDESCRIPTION");for _,x:=range list{state:="enabled";if x.Disabled{state="disabled"};fmt.Fprintf(tw,"%s\t%s\t%s\t%s\t%s\n",x.Name,x.Version,x.Digest,state,x.Description)};tw.Flush()})
 }})
 add(&Command{Name:"recipe publish",Args:[]Arg{{Name:"FILE",Help:"JSON file containing one immutable recipe definition"}},Summary:"publish a new immutable job recipe version",Output:"{definition, digest, disabled}",Mutates:true,Examples:[]string{"messh recipe publish recipe.json"},Run:func(c *Context)error{
  b,err:=os.ReadFile(c.Args[0]);if err!=nil{return err};var d recipes.Definition;dec:=json.NewDecoder(bytes.NewReader(b));dec.DisallowUnknownFields();if err=dec.Decode(&d);err!=nil{return err};var extra any;if err=dec.Decode(&extra);err!=io.EOF{if err==nil{return fmt.Errorf("recipe file contains multiple JSON values")};return err};cl,_,err:=c.Node();if err!=nil{return err};var out recipes.Recipe;if err=cl.Do(context.Background(),http.MethodPost,"/v1/recipes",d,&out);err!=nil{return err};return c.Emit(out,func(w io.Writer){fmt.Fprintf(w,"Published %s@%s\nDigest: %s\n",out.Name,out.Version,out.Digest)})
 }})
 add(&Command{Name:"recipe get",Args:[]Arg{{Name:"NAME"},{Name:"VERSION"},{Name:"DIGEST",Optional:true}},Summary:"show a published job recipe and its schema",Output:"{definition, digest, disabled}",Examples:[]string{"messh recipe get train 1.0.0 sha256..."},Run:func(c *Context)error{
  path:="/v1/recipes/"+url.PathEscape(c.Args[0])+"/"+url.PathEscape(c.Args[1]);if len(c.Args)>2{path+="?digest="+url.QueryEscape(c.Args[2])};cl,_,err:=c.Node();if err!=nil{return err};var out recipes.Recipe;if err=cl.Do(context.Background(),http.MethodGet,path,nil,&out);err!=nil{return err};return c.Emit(out,func(w io.Writer){fmt.Fprintf(w,"%s@%s\nDigest: %s\nCommand: %s\n",out.Name,out.Version,out.Digest,out.Command);fmt.Fprintln(w,"Parameters:");var schema any;if json.Unmarshal(out.Parameters,&schema)==nil{b,_:=json.MarshalIndent(schema,"  ","  ");fmt.Fprintf(w,"  %s\n",b)}})
 }})
 add(&Command{Name:"recipe disable",Args:[]Arg{{Name:"NAME"},{Name:"VERSION"}},Summary:"disable a recipe version for future submissions",Output:"{definition, digest, disabled}",Mutates:true,Examples:[]string{"messh recipe disable train 1.0.0"},Run:func(c *Context)error{
  path:="/v1/recipes/"+url.PathEscape(c.Args[0])+"/"+url.PathEscape(c.Args[1])+"/disable";cl,_,err:=c.Node();if err!=nil{return err};var out recipes.Recipe;if err=cl.Do(context.Background(),http.MethodPost,path,nil,&out);err!=nil{return err};return c.Emit(out,func(w io.Writer){fmt.Fprintf(w,"Disabled %s@%s for new submissions.\n",out.Name,out.Version)})
 }})
}

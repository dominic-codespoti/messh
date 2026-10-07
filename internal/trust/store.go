// Package trust stores owner-approved device identities.
package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"messh/internal/state"
	"os"
	"sort"
	"sync"
	"time"
)

// ErrNotFound means the requested device is not trusted.
var ErrNotFound = errors.New("trusted device not found")

const documentVersion = 1

// Device is one trusted device identity.
type Device struct {
	DeviceID string    `json:"device_id"`
	Device   string    `json:"device"`
	Created  time.Time `json:"created"`
}

type document struct {
	Version int      `json:"version"`
	Devices []Device `json:"devices"`
}

// Store keeps device trust in a durable owner-local file.
type Store struct {
	mu      sync.RWMutex
	file    string
	devices map[string]Device
}

// New loads a trust store. A missing file represents an empty policy.
func New(file string) (*Store, error) {
	s := &Store{file: file, devices: make(map[string]Device)}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trust store: %w", err)
	}
	var doc document
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode trust store: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode trust store: trailing JSON value")
		}
		return nil, fmt.Errorf("decode trust store: %w", err)
	}
	if doc.Version != documentVersion {
		return nil, fmt.Errorf("unsupported trust store version %d", doc.Version)
	}
	for _, device := range doc.Devices {
		if device.DeviceID == "" || device.Device == "" || device.Created.IsZero() {
			return nil, errors.New("invalid device in trust store")
		}
		if _, exists := s.devices[device.DeviceID]; exists {
			return nil, fmt.Errorf("duplicate device ID %q in trust store", device.DeviceID)
		}
		s.devices[device.DeviceID] = device
	}
	return s, nil
}

// Add trusts deviceID. Re-adding an existing ID is idempotent and preserves
// the original device record.
func (s *Store) Add(deviceID, device string) (Device, error) {
	if deviceID == "" || device == "" {
		return Device{}, errors.New("device ID and device name are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.devices[deviceID]; ok {
		return existing, nil
	}
	entry := Device{DeviceID: deviceID, Device: device, Created: time.Now().UTC()}
	next := make(map[string]Device, len(s.devices)+1)
	for id, current := range s.devices {
		next[id] = current
	}
	next[deviceID] = entry
	if err := s.save(next); err != nil {
		return Device{}, err
	}
	s.devices = next
	return entry, nil
}

// Remove revokes trust for deviceID and persists the change before applying it.
func (s *Store) Remove(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[deviceID]; !ok {
		return ErrNotFound
	}
	next := make(map[string]Device, len(s.devices)-1)
	for id, current := range s.devices {
		if id != deviceID {
			next[id] = current
		}
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.devices = next
	return nil
}

// List returns a copy of trusted devices sorted by immutable device ID.
func (s *Store) List() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	devices := make([]Device, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].DeviceID < devices[j].DeviceID })
	return devices
}

// Allows reports whether deviceID is trusted. The empty ID is never trusted.
func (s *Store) Allows(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.devices[deviceID]
	s.mu.RUnlock()
	return ok
}

func (s *Store) save(devices map[string]Device) error {
	entries := make([]Device, 0, len(devices))
	for _, device := range devices {
		entries = append(entries, device)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].DeviceID < entries[j].DeviceID })
	data, err := json.MarshalIndent(document{Version: documentVersion, Devices: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust store: %w", err)
	}
	data = append(data, '\n')
	if err := state.WriteFileAtomic(s.file, data, 0o600); err != nil {
		return fmt.Errorf("write trust store: %w", err)
	}
	return nil
}

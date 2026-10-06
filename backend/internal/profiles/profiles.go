// Package profiles stores several client configs ("servers") and remembers which one is active.
//
// Everything lives in a single JSON file that is replaced atomically, so a crash never leaves
// the index and the configs out of sync.
package profiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("profile not found")

// Profile is one saved client config.
type Profile struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	Created time.Time `json:"created"`
	Config  string    `json:"config"`
}

type state struct {
	Active   string     `json:"active"`
	Backup   string     `json:"backup,omitempty"` // used while the active profile is down
	Profiles []*Profile `json:"profiles"`
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
}

// Open loads path. A missing file is an empty store; if legacyConf exists and the store is new,
// it is imported as the first (active) profile and removed.
func Open(path, legacyConf string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		if legacyConf == "" {
			break
		}
		text, err := os.ReadFile(legacyConf)
		if err != nil {
			break
		}
		p := &Profile{ID: newID(), Name: "Default", Source: "migrated", Created: time.Now().UTC(), Config: string(text)}
		s.st = state{Active: p.ID, Profiles: []*Profile{p}}
		if err := s.save(); err != nil {
			return nil, err
		}
		os.Remove(legacyConf)
	default:
		return nil, err
	}
	return s, nil
}

// List returns copies of all profiles and the active ID.
func (s *Store) List() ([]Profile, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Profile, len(s.st.Profiles))
	for i, p := range s.st.Profiles {
		out[i] = *p
	}
	return out, s.st.Active
}

// Get returns a profile by ID.
func (s *Store) Get(id string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.find(id); p != nil {
		return *p, nil
	}
	return Profile{}, ErrNotFound
}

// Active returns the active profile.
func (s *Store) Active() (Profile, error) {
	s.mu.Lock()
	id := s.st.Active
	s.mu.Unlock()
	return s.Get(id)
}

// Backup returns the backup profile, or ErrNotFound if none is set.
func (s *Store) Backup() (Profile, error) {
	s.mu.Lock()
	id := s.st.Backup
	s.mu.Unlock()
	return s.Get(id)
}

// SetBackup makes a profile the backup of the active one; an empty id clears it.
func (s *Store) SetBackup(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		if s.find(id) == nil {
			return ErrNotFound
		}
		if id == s.st.Active {
			return errors.New("the active server cannot be its own backup")
		}
	}
	prev := s.st.Backup
	s.st.Backup = id
	if err := s.save(); err != nil {
		s.st.Backup = prev
		return err
	}
	return nil
}

// Add saves a new profile and, if activate is set, makes it active.
func (s *Store) Add(name, source, config string, activate bool) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Profile{ID: newID(), Name: strings.TrimSpace(name), Source: source, Created: time.Now().UTC(), Config: config}
	if p.Name == "" {
		p.Name = "Server " + p.ID[:4]
	}
	prev := s.st
	s.st.Profiles = append(append([]*Profile(nil), s.st.Profiles...), p)
	if activate || s.st.Active == "" {
		s.st.Active = p.ID
	}
	if err := s.save(); err != nil {
		s.st = prev
		return Profile{}, err
	}
	return *p, nil
}

// SetActive selects a profile. If it was the backup, the previously active profile becomes the backup.
func (s *Store) SetActive(id string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	if p == nil {
		return Profile{}, ErrNotFound
	}
	prev := s.st
	if id == s.st.Backup {
		// Selecting the backup swaps the roles.
		s.st.Backup = s.st.Active
	}
	s.st.Active = id
	if err := s.save(); err != nil {
		s.st = prev
		return Profile{}, err
	}
	return *p, nil
}

// Rename changes a profile's display name.
func (s *Store) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	if p == nil {
		return ErrNotFound
	}
	prev := p.Name
	p.Name = name
	if err := s.save(); err != nil {
		p.Name = prev
		return err
	}
	return nil
}

// Delete removes a profile. If it was active, the backup (or else the first remaining profile)
// becomes active; wasActive reports that so the caller can stop the tunnel.
func (s *Store) Delete(id string) (wasActive bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.st
	var rest []*Profile
	for _, p := range s.st.Profiles {
		if p.ID != id {
			rest = append(rest, p)
		}
	}
	if len(rest) == len(s.st.Profiles) {
		return false, ErrNotFound
	}
	s.st.Profiles = rest
	if s.st.Backup == id {
		s.st.Backup = ""
	}
	if wasActive = s.st.Active == id; wasActive {
		// The backup, if any, takes over; otherwise the first remaining profile.
		s.st.Active, s.st.Backup = s.st.Backup, ""
		if s.st.Active == "" && len(rest) > 0 {
			s.st.Active = rest[0].ID
		}
	}
	if err := s.save(); err != nil {
		s.st = prev
		return false, err
	}
	return wasActive, nil
}

func (s *Store) find(id string) *Profile {
	for _, p := range s.st.Profiles {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

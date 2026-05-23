package state

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mattconzen/microvm/config"
)

const (
	bucketSandboxes = "sandboxes"
	bucketSnapshots = "snapshots"
)

var ErrNotFound = errors.New("not found")

type Sandbox struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	SessionID string    `json:"session_id"`
	Image     string    `json:"image,omitempty"`
	Name      string    `json:"name,omitempty"`
	CPUs      float64   `json:"cpus,omitempty"`
	MemoryMB  int       `json:"memory_mb,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type Snapshot struct {
	ID              string    `json:"id"`
	SandboxID       string    `json:"sandbox_id"`
	Provider        string    `json:"provider"`
	TargetSessionID string    `json:"target_session_id"`
	Kind            string    `json:"kind"` // "alias" on AWS
	Name            string    `json:"name,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type Store struct {
	db *bolt.DB
}

func Open() (*Store, error) {
	d, err := config.Dir()
	if err != nil {
		return nil, err
	}
	if err := ensureDir(d); err != nil {
		return nil, err
	}
	path := filepath.Join(d, "state.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{bucketSandboxes, bucketSnapshots} {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PutSandbox(sb Sandbox) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketSandboxes))
		raw, err := json.Marshal(sb)
		if err != nil {
			return err
		}
		return b.Put([]byte(sb.ID), raw)
	})
}

func (s *Store) GetSandbox(id string) (Sandbox, error) {
	var out Sandbox
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketSandboxes)).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func (s *Store) DeleteSandbox(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSandboxes)).Delete([]byte(id))
	})
}

func (s *Store) ListSandboxes() ([]Sandbox, error) {
	var out []Sandbox
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSandboxes)).ForEach(func(_, v []byte) error {
			var sb Sandbox
			if err := json.Unmarshal(v, &sb); err != nil {
				return err
			}
			out = append(out, sb)
			return nil
		})
	})
	return out, err
}

func (s *Store) PutSnapshot(sn Snapshot) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(sn)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketSnapshots)).Put([]byte(sn.ID), raw)
	})
}

func (s *Store) GetSnapshot(id string) (Snapshot, error) {
	var out Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketSnapshots)).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func (s *Store) ListSnapshots() ([]Snapshot, error) {
	var out []Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSnapshots)).ForEach(func(_, v []byte) error {
			var sn Snapshot
			if err := json.Unmarshal(v, &sn); err != nil {
				return err
			}
			out = append(out, sn)
			return nil
		})
	})
	return out, err
}

// NewSandboxID mints an "mvm_" prefixed lowercase base32 ID.
func NewSandboxID() string {
	return "mvm_" + randB32(8)
}

// NewSnapshotID mints a "snp_" prefixed lowercase base32 ID.
func NewSnapshotID() string {
	return "snp_" + randB32(8)
}

// NewSessionID mints an opaque session id passed to AgentCore as runtimeSessionId.
// AgentCore requires sessionId to be at least 33 characters long.
func NewSessionID() string {
	return "mvm-sess-" + randB32(28)
}

func randB32(n int) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	enc = strings.ToLower(enc)
	if len(enc) < n {
		return enc
	}
	return enc[:n]
}

func ensureDir(d string) error {
	return mkdirAll(d, 0o700)
}

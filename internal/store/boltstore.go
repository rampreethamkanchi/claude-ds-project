// Package store — boltstore.go
//
// BoltDB-backed implementations of hashicorp/raft's LogStore and StableStore interfaces.
//
// LogStore saves Raft log entries to disk so they survive process restarts.
// StableStore saves small key-value pairs (current term, last vote) used by Raft.
//
// We use two separate BoltDB buckets in the same database file:
//   - "raft-log"    → Raft log entries (indexed by uint64 log index)
//   - "raft-stable" → Raft stable key-value pairs
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketLog    = []byte("raft-log")    // bucket for Raft log entries
	bucketStable = []byte("raft-stable") // bucket for Raft stable store
)

// BoltStore implements both raft.LogStore and raft.StableStore using BoltDB.
type BoltStore struct {
	db *bolt.DB
}

// NewBoltStore opens (or creates) a BoltDB file at the given path and
// initialises the required buckets.
func NewBoltStore(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("boltstore: open %q: %w", path, err)
	}

	// Create buckets if they don't exist.
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketLog); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketStable); err != nil {
			return err
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("boltstore: init buckets: %w", err)
	}

	return &BoltStore{db: db}, nil
}

// Close closes the underlying BoltDB database.
func (s *BoltStore) Close() error {
	return s.db.Close()
}

// ─────────────────────────────────────────────
// raft.LogStore implementation
// ─────────────────────────────────────────────

// FirstIndex returns the index of the first log entry stored.
func (s *BoltStore) FirstIndex() (uint64, error) {
	var idx uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketLog).Cursor()
		k, _ := c.First()
		if k != nil {
			idx = bytesToUint64(k)
		}
		return nil
	})
	return idx, err
}

// LastIndex returns the index of the last log entry stored.
func (s *BoltStore) LastIndex() (uint64, error) {
	var idx uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketLog).Cursor()
		k, _ := c.Last()
		if k != nil {
			idx = bytesToUint64(k)
		}
		return nil
	})
	return idx, err
}

// GetLog retrieves a log entry by index, writing it into `out`.
func (s *BoltStore) GetLog(index uint64, out *raft.Log) error {
	return s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketLog).Get(uint64ToBytes(index))
		if v == nil {
			return raft.ErrLogNotFound
		}
		return json.Unmarshal(v, out)
	})
}

// StoreLog stores a single log entry.
func (s *BoltStore) StoreLog(log *raft.Log) error {
	return s.StoreLogs([]*raft.Log{log})
}

// StoreLogs stores multiple log entries in a single batch transaction.
func (s *BoltStore) StoreLogs(logs []*raft.Log) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketLog)
		for _, l := range logs {
			data, err := json.Marshal(l)
			if err != nil {
				return fmt.Errorf("marshal log %d: %w", l.Index, err)
			}
			if err := b.Put(uint64ToBytes(l.Index), data); err != nil {
				return fmt.Errorf("put log %d: %w", l.Index, err)
			}
		}
		return nil
	})
}

// DeleteRange deletes log entries in the inclusive range [min, max].
// Called by Raft during log compaction after a snapshot is taken.
func (s *BoltStore) DeleteRange(min, max uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketLog)
		c := b.Cursor()
		for k, _ := c.Seek(uint64ToBytes(min)); k != nil; k, _ = c.Next() {
			if bytesToUint64(k) > max {
				break
			}
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

// ─────────────────────────────────────────────
// raft.StableStore implementation
// ─────────────────────────────────────────────

// Set stores a key-value pair (used by Raft for current term and last vote).
func (s *BoltStore) Set(key []byte, val []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStable).Put(key, val)
	})
}

// Get retrieves a value by key. Returns an empty slice if not found.
func (s *BoltStore) Get(key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketStable).Get(key)
		if v != nil {
			val = make([]byte, len(v))
			copy(val, v)
		}
		return nil
	})
	return val, err
}

// SetUint64 stores a uint64 value (used by Raft for the current term).
func (s *BoltStore) SetUint64(key []byte, val uint64) error {
	return s.Set(key, uint64ToBytes(val))
}

// GetUint64 retrieves a uint64 value. Returns 0 if not found.
func (s *BoltStore) GetUint64(key []byte) (uint64, error) {
	val, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if len(val) == 0 {
		return 0, nil
	}
	if len(val) != 8 {
		return 0, errors.New("invalid uint64 value in stable store")
	}
	return bytesToUint64(val), nil
}

// ─────────────────────────────────────────────
// Byte encoding helpers
// ─────────────────────────────────────────────

// uint64ToBytes encodes a uint64 as big-endian 8 bytes.
// Big-endian is required so BoltDB's lexicographic cursor ordering
// matches numeric ordering for log indices.
func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// bytesToUint64 decodes a big-endian 8-byte slice to uint64.
func bytesToUint64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}

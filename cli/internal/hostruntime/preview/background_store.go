package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const backgroundStoreSchema = "paperboat.background-previews/v1"

// BackgroundRecord is what a restarted host runtime needs to resume one
// background preview with the same URL: the secret-free dispatch the server sent
// and the operating-system boot it belongs to. A preview is never restored after
// a reboot, so a record from another boot is dropped.
type BackgroundRecord struct {
	OSBootID string          `json:"os_boot_id"`
	Request  DispatchRequest `json:"request"`
}

// BackgroundStore keeps the background previews of this machine in one small
// owner-only file. It holds no credentials.
type BackgroundStore struct {
	path string
	mu   sync.Mutex
}

type backgroundFile struct {
	Schema  string             `json:"schema"`
	Records []BackgroundRecord `json:"records"`
}

func NewBackgroundStore(path string) *BackgroundStore { return &BackgroundStore{path: path} }

// Save adds or replaces the record for the request's operation.
func (s *BackgroundStore) Save(record BackgroundRecord) error {
	return s.update(func(records map[string]BackgroundRecord) {
		records[record.Request.OperationID] = record
	})
}

// Remove forgets one operation; removing an unknown operation is not an error.
func (s *BackgroundStore) Remove(operationID string) error {
	return s.update(func(records map[string]BackgroundRecord) { delete(records, operationID) })
}

// Load returns every stored record, oldest operation first. A missing file is empty.
func (s *BackgroundStore) Load() ([]BackgroundRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	return sortedBackgroundRecords(records), nil
}

func (s *BackgroundStore) update(change func(map[string]BackgroundRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.read()
	if err != nil {
		// An unreadable file holds nothing resumable; start over rather than keep failing.
		records = map[string]BackgroundRecord{}
	}
	change(records)
	return s.write(sortedBackgroundRecords(records))
}

func (s *BackgroundStore) read() (map[string]BackgroundRecord, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]BackgroundRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read background previews: %w", err)
	}
	var file backgroundFile
	if err := json.Unmarshal(data, &file); err != nil || file.Schema != backgroundStoreSchema {
		return nil, fmt.Errorf("background previews file is not readable")
	}
	records := make(map[string]BackgroundRecord, len(file.Records))
	for _, record := range file.Records {
		records[record.Request.OperationID] = record
	}
	return records, nil
}

func (s *BackgroundStore) write(records []BackgroundRecord) error {
	if len(records) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove background previews: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(backgroundFile{Schema: backgroundStoreSchema, Records: records})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create background previews directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".background-previews-*")
	if err != nil {
		return fmt.Errorf("write background previews: %w", err)
	}
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("write background previews: %w", err)
	}
	if err := os.Rename(temp.Name(), s.path); err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("write background previews: %w", err)
	}
	return nil
}

func sortedBackgroundRecords(records map[string]BackgroundRecord) []BackgroundRecord {
	sorted := make([]BackgroundRecord, 0, len(records))
	for _, record := range records {
		sorted = append(sorted, record)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Request.OperationID < sorted[j].Request.OperationID })
	return sorted
}

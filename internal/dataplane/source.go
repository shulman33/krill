package dataplane

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/shulman33/krill/internal/ext4"
)

// DataSource yields the guest database's bytes as the host sees them.
// precise=false is the fast poll path: metadata may lag (bounded staleness,
// and the WAL's own checksum chain rejects anything torn). precise=true
// replays the filesystem journal first and must be used at decision points:
// sync-ack holds, freeze catch-up, checkpoint materialization.
type DataSource interface {
	WAL(precise bool) ([]byte, error)
	DB(precise bool) ([]byte, error)
}

// Ext4Source reads the app's SQLite files out of its data-disk image — the
// production source: the guest writes through virtio, krilld reads the
// backing file. Missing files (fresh app) read as empty.
type Ext4Source struct {
	ImagePath string
	DBPath    string // path inside the image, e.g. "/app.db"
}

func (s *Ext4Source) WAL(precise bool) ([]byte, error) {
	return s.read(s.DBPath+"-wal", precise)
}

func (s *Ext4Source) DB(precise bool) ([]byte, error) {
	return s.read(s.DBPath, precise)
}

func (s *Ext4Source) read(path string, precise bool) ([]byte, error) {
	f, err := os.Open(s.ImagePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fs, err := ext4.Open(f, ext4.Options{SkipJournal: !precise})
	if err != nil {
		return nil, err
	}
	b, err := fs.ReadFile(path)
	if errors.Is(err, ext4.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// DirSource reads the SQLite files from a plain directory: unit tests and
// the gate tooling point it at a database written directly on the host.
type DirSource struct {
	Dir  string
	Base string // e.g. "app.db"
}

func (s *DirSource) WAL(bool) ([]byte, error) { return readOrNil(filepath.Join(s.Dir, s.Base+"-wal")) }
func (s *DirSource) DB(bool) ([]byte, error)  { return readOrNil(filepath.Join(s.Dir, s.Base)) }

func readOrNil(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

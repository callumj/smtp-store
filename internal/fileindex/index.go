package fileindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"smtp-store/internal/classify"
)

// Index stores local metadata for files whose bytes live on the storage mount.
type Index struct {
	db     *sql.DB
	root   string
	logger *slog.Logger
}

// File is one indexed storage item.
type File struct {
	RelativePath string
	AbsPath      string
	Recipient    string
	Name         string
	Size         int64
	ModTime      time.Time
	DetectState  string
	HasPerson    bool
	HasAnimal    bool
	HasVehicle   bool
	DetectLabels []string
}

// Open initializes a SQLite index at path.
func Open(path, storageRoot string, logger *slog.Logger) (*Index, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("index path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}
	rootAbs, err := filepath.Abs(storageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	idx := &Index{db: db, root: rootAbs, logger: logger}
	if err := idx.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

func (i *Index) init() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS files (
			relative_path TEXT PRIMARY KEY,
			abs_path TEXT NOT NULL,
			recipient TEXT NOT NULL,
			name TEXT NOT NULL,
			size INTEGER NOT NULL,
			mod_time_unix INTEGER NOT NULL,
			is_video INTEGER NOT NULL,
			detect_state TEXT NOT NULL DEFAULT '',
			has_person INTEGER NOT NULL DEFAULT 0,
			has_animal INTEGER NOT NULL DEFAULT 0,
			has_vehicle INTEGER NOT NULL DEFAULT 0,
			detect_labels_json TEXT NOT NULL DEFAULT '[]',
			updated_at_unix INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS files_mod_time_idx ON files(mod_time_unix DESC)`,
		`CREATE INDEX IF NOT EXISTS files_recipient_idx ON files(recipient)`,
	}
	for _, stmt := range stmts {
		if _, err := i.db.Exec(stmt); err != nil {
			return fmt.Errorf("initialize index: %w", err)
		}
	}
	if err := i.ensureColumn("files", "has_vehicle", `ALTER TABLE files ADD COLUMN has_vehicle INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

func (i *Index) ensureColumn(table, column, alterStmt string) error {
	rows, err := i.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = i.db.Exec(alterStmt)
	return err
}

// Backfill walks the storage tree and refreshes index metadata.
func (i *Index) Backfill(ctx context.Context) error {
	if i == nil {
		return nil
	}
	start := time.Now()
	var scanned, indexed int
	err := filepath.WalkDir(i.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() || classify.IsDetectionSidecarPath(path) {
			return nil
		}
		scanned++
		if err := i.UpsertPath(path); err != nil {
			if i.logger != nil {
				i.logger.Warn("failed indexing file", "path", path, "error", err)
			}
			return nil
		}
		indexed++
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if i.logger != nil {
		i.logger.Info("file index backfill completed", "scanned", scanned, "indexed", indexed, "duration", time.Since(start).String())
	}
	return err
}

// UpsertPath refreshes metadata for one file path.
func (i *Index) UpsertPath(path string) error {
	if i == nil {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if classify.IsDetectionSidecarPath(abs) {
		return nil
	}
	if abs != i.root && !strings.HasPrefix(abs, i.root+string(os.PathSeparator)) {
		return fmt.Errorf("path outside storage root: %s", path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return nil
	}
	rel, err := filepath.Rel(i.root, abs)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	item := File{
		RelativePath: rel,
		AbsPath:      abs,
		Recipient:    recipientFromRel(rel),
		Name:         filepath.Base(abs),
		Size:         info.Size(),
		ModTime:      info.ModTime(),
	}
	if classify.IsVideoPath(abs) {
		if sidecar, err := classify.LoadSidecar(abs); err == nil {
			applySidecar(&item, sidecar)
		} else {
			item.DetectState = classify.StatePending
		}
	}
	return i.upsert(item, classify.IsVideoPath(abs))
}

// UpsertSidecar refreshes detection metadata for a video that already exists in the index.
func (i *Index) UpsertSidecar(videoPath string, sidecar classify.Sidecar) error {
	if i == nil {
		return nil
	}
	if err := i.UpsertPath(videoPath); err != nil {
		return err
	}
	rel, err := filepath.Rel(i.root, videoPath)
	if err != nil {
		return err
	}
	item := File{RelativePath: filepath.ToSlash(rel)}
	applySidecar(&item, &sidecar)
	labels, _ := json.Marshal(item.DetectLabels)
	_, err = i.db.Exec(`UPDATE files
		SET detect_state = ?, has_person = ?, has_animal = ?, has_vehicle = ?, detect_labels_json = ?, updated_at_unix = ?
		WHERE relative_path = ?`,
		item.DetectState, boolInt(item.HasPerson), boolInt(item.HasAnimal), boolInt(item.HasVehicle), string(labels), time.Now().Unix(), item.RelativePath)
	return err
}

func (i *Index) upsert(item File, isVideo bool) error {
	labels, err := json.Marshal(item.DetectLabels)
	if err != nil {
		return err
	}
	_, err = i.db.Exec(`INSERT INTO files (
			relative_path, abs_path, recipient, name, size, mod_time_unix, is_video,
			detect_state, has_person, has_animal, has_vehicle, detect_labels_json, updated_at_unix
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(relative_path) DO UPDATE SET
			abs_path = excluded.abs_path,
			recipient = excluded.recipient,
			name = excluded.name,
			size = excluded.size,
			mod_time_unix = excluded.mod_time_unix,
			is_video = excluded.is_video,
			detect_state = excluded.detect_state,
			has_person = excluded.has_person,
			has_animal = excluded.has_animal,
			has_vehicle = excluded.has_vehicle,
			detect_labels_json = excluded.detect_labels_json,
			updated_at_unix = excluded.updated_at_unix`,
		item.RelativePath, item.AbsPath, item.Recipient, item.Name, item.Size, item.ModTime.Unix(), boolInt(isVideo),
		item.DetectState, boolInt(item.HasPerson), boolInt(item.HasAnimal), boolInt(item.HasVehicle), string(labels), time.Now().Unix())
	return err
}

// Recipients returns indexed top-level recipient folders.
func (i *Index) Recipients() ([]string, error) {
	rows, err := i.db.Query(`SELECT DISTINCT recipient FROM files ORDER BY lower(recipient)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			return nil, err
		}
		out = append(out, recipient)
	}
	return out, rows.Err()
}

// Recent returns the most recently modified indexed files.
func (i *Index) Recent(limit int) ([]File, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := i.db.Query(`SELECT relative_path, abs_path, recipient, name, size, mod_time_unix,
			detect_state, has_person, has_animal, has_vehicle, detect_labels_json
		FROM files
		ORDER BY mod_time_unix DESC, relative_path DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]File, 0, limit)
	for rows.Next() {
		var item File
		var modUnix int64
		var hasPerson, hasAnimal, hasVehicle int
		var labelsJSON string
		if err := rows.Scan(&item.RelativePath, &item.AbsPath, &item.Recipient, &item.Name, &item.Size, &modUnix, &item.DetectState, &hasPerson, &hasAnimal, &hasVehicle, &labelsJSON); err != nil {
			return nil, err
		}
		item.ModTime = time.Unix(modUnix, 0)
		item.HasPerson = hasPerson != 0
		item.HasAnimal = hasAnimal != 0
		item.HasVehicle = hasVehicle != 0
		_ = json.Unmarshal([]byte(labelsJSON), &item.DetectLabels)
		out = append(out, item)
	}
	return out, rows.Err()
}

func applySidecar(item *File, sidecar *classify.Sidecar) {
	if sidecar == nil {
		return
	}
	item.DetectState = classify.DetectionStatus(sidecar)
	item.HasPerson = sidecar.HasPerson
	item.HasAnimal = sidecar.HasAnimal
	item.HasVehicle = sidecar.HasVehicle
	item.DetectLabels = detectionLabels(sidecar)
}

func detectionLabels(sidecar *classify.Sidecar) []string {
	if sidecar == nil || len(sidecar.Detections) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(sidecar.Detections))
	out := make([]string, 0, len(sidecar.Detections))
	for _, d := range sidecar.Detections {
		label := strings.TrimSpace(strings.ToLower(d.Label))
		if label == "" || isGenericDetectionLabel(label, d.Category) {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func isGenericDetectionLabel(label, category string) bool {
	category = strings.TrimSpace(strings.ToLower(category))
	if label == category {
		return true
	}
	switch label {
	case "person", "people", "human", "animal", "vehicle", "car":
		return true
	default:
		return false
	}
}

func recipientFromRel(rel string) string {
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return "unknown"
	}
	return parts[0]
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type config struct {
	url          string
	provider     string
	subdirectory string
	token        string
}

var nameRe = regexp.MustCompile(`^vaultwarden-.*\.zip$`)
var dateRe = regexp.MustCompile(`^vaultwarden-(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})\.zip$`)

func main() {
	if err := backup(); err != nil {
		logMsg("error", "error", err.Error())
		os.Exit(1)
	}
}

func backup() error {
	password := os.Getenv("BACKUP_PASSWORD")
	if password == "" {
		return fmt.Errorf("BACKUP_PASSWORD is not set")
	}

	cfg := config{
		url:          envOr("BACKIO_URL", "http://backio:8080"),
		provider:     envOr("BACKIO_PROVIDER", "gdrive"),
		subdirectory: os.Getenv("BACKIO_SUBDIRECTORY"),
		token:        os.Getenv("BACKUP_TOKEN"),
	}
	if cfg.subdirectory == "" {
		return fmt.Errorf("BACKIO_SUBDIRECTORY is not set")
	}

	log("backup", "Starting backup")

	ts := timestamp()
	tmpdir := fmt.Sprintf("/tmp/bw-%s", ts)
	archiveName := fmt.Sprintf("vaultwarden-%s.zip", ts)
	archive := filepath.Join("/backups", archiveName)

	if err := os.MkdirAll(tmpdir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log("sqlite_backup", fmt.Sprintf("Backing up database to %s", tmpdir))
	if err := run("sqlite3", "/data/db.sqlite3", ".backup "+filepath.Join(tmpdir, "db.sqlite3")); err != nil {
		return err
	}

	// Optional single files: copy if present, ignore if missing.
	// rsa_key.* covers PEM and DER key files across Vaultwarden versions.
	rsaKeys, _ := filepath.Glob("/data/rsa_key.*")
	for _, src := range append(rsaKeys, "/data/config.json") {
		if exists(src) {
			name := filepath.Base(src)
			log("copy_files", "Copying "+name)
			if err := run("cp", src, filepath.Join(tmpdir, name)); err != nil {
				return err
			}
		}
	}

	// directories: copy if present, ignore if missing.
	for _, d := range []string{"attachments", "sends"} {
		if exists("/data/" + d) {
			log("copy_files", "Copying "+d)
			if err := run("cp", "-r", "/data/"+d, filepath.Join(tmpdir, d)); err != nil {
				return err
			}
		}
	}

	log("compress", "Creating archive "+archiveName)
	if err := run("7z", "a", "-tzip", "-p"+password, "-mem=AES256", "-mx=1", archive, tmpdir); err != nil {
		return err
	}

	log("backup", "Local backup complete: "+archiveName)

	if err := uploadToBackio(archive, archiveName, cfg); err != nil {
		return err
	}

	cleanupLocalBackups()
	cleanupRemoteBackups(cfg)

	return nil
}

type backupEntry struct {
	name string
	date time.Time
}

func cleanupLocalBackups() {
	log("cleanup", "Running local retention policy: 3 recent, 1 previous day, 1 weekly, 1 monthly")

	names, err := listLocalBackups()
	if err != nil {
		logError("cleanup", "Failed to list local backups: "+err.Error())
		return
	}

	backups := parseEntries(names)
	keep := selectBackupsToKeep(backups)

	logMsg("info", "cleanup", fmt.Sprintf("Keeping %d of %d: %s", len(keep), len(backups), joinKeys(keep)))

	for _, name := range names {
		if !keep[name] {
			log("cleanup", "Removing local: "+name)
			if err := os.Remove(filepath.Join("/backups", name)); err != nil {
				logError("cleanup", fmt.Sprintf("Failed to remove local %s: %s", name, err.Error()))
			}
		}
	}
}

func cleanupRemoteBackups(cfg config) {
	log("gdrive", "Running remote retention policy: 3 recent, 1 previous day, 1 weekly, 1 monthly")

	names, err := listBackioBackups(cfg)
	if err != nil {
		logError("gdrive", "Failed to list remote backups: "+err.Error())
		return
	}
	if len(names) == 0 {
		return
	}

	backups := parseEntries(names)
	keep := selectBackupsToKeep(backups)

	logMsg("info", "gdrive", fmt.Sprintf("Keeping %d of %d: %s", len(keep), len(backups), joinKeys(keep)))

	for _, name := range names {
		if !keep[name] {
			log("gdrive", "Removing remote: "+name)
			if err := deleteBackioBackup(name, cfg); err != nil {
				logError("gdrive", fmt.Sprintf("Failed to delete remote %s: %s", name, err.Error()))
			}
		}
	}
}

func uploadToBackio(archive, archiveName string, cfg config) error {
	if cfg.token == "" {
		log("gdrive", "Skipping upload: BACKUP_TOKEN not set")
		return nil
	}

	log("gdrive", "Uploading "+archiveName+" to "+cfg.provider)

	fileBytes, err := os.ReadFile(archive)
	if err != nil {
		return err
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("backup", archiveName)
	if err != nil {
		return err
	}
	if _, err := part.Write(fileBytes); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"name":         archiveName,
		"subdirectory": cfg.subdirectory,
		"provider":     cfg.provider,
	} {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.url+"/backup", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBytes, _ := io.ReadAll(resp.Body)
	responseText := string(responseBytes)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Upload failed (%d): %s", resp.StatusCode, responseText)
	}

	var result struct {
		Status      string `json:"status"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(responseBytes, &result); err != nil {
		return fmt.Errorf("Invalid response: %s", responseText)
	}
	if result.Status != "ok" {
		return fmt.Errorf("Backup failed: %s", responseText)
	}

	log("gdrive", "Remote backup success: "+result.Destination)
	return nil
}

func listBackioBackups(cfg config) ([]string, error) {
	if cfg.token == "" {
		return nil, nil
	}

	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)

	req, err := http.NewRequest(http.MethodGet, cfg.url+"/backup?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		log("gdrive", "Skipping remote backup list: insufficient permissions")
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("List failed (%d): %s", resp.StatusCode, string(text))
	}

	var items []struct {
		Name  string `json:"Name"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var names []string
	for _, item := range items {
		if !item.IsDir && nameRe.MatchString(item.Name) {
			names = append(names, item.Name)
		}
	}
	return names, nil
}

func deleteBackioBackup(name string, cfg config) error {
	if cfg.token == "" {
		return nil
	}

	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)
	params.Set("name", name)

	req, err := http.NewRequest(http.MethodDelete, cfg.url+"/backup?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		logError("gdrive", "Skipping remote delete "+name+": insufficient permissions")
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Delete failed (%d): %s", resp.StatusCode, string(text))
	}
	return nil
}

func listLocalBackups() ([]string, error) {
	entries, err := os.ReadDir("/backups")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if nameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// parseEntries maps names to entries with parsed dates, drops unparseable
// names, and sorts newest first.
func parseEntries(names []string) []backupEntry {
	var backups []backupEntry
	for _, name := range names {
		if date, ok := parseDateFromName(name); ok {
			backups = append(backups, backupEntry{name: name, date: date})
		}
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].date.After(backups[j].date)
	})
	return backups
}

func parseDateFromName(name string) (time.Time, bool) {
	m := dateRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	n := make([]int, 6)
	for i := range n {
		fmt.Sscanf(m[i+1], "%d", &n[i])
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.UTC), true
}

// selectBackupsToKeep expects backups sorted newest first.
func selectBackupsToKeep(backups []backupEntry) map[string]bool {
	keep := make(map[string]bool)
	now := time.Now().UTC()

	// 3 most recent
	for i := 0; i < len(backups) && i < 3; i++ {
		keep[backups[i].name] = true
	}

	if len(backups) == 0 {
		return keep
	}
	oldest := backups[len(backups)-1]
	slot := func(name string) { keep[name] = true }

	// 1 from the previous calendar day, else oldest
	yesterday := now.Add(-24 * time.Hour).Format("2006-01-02")
	prevDay := ""
	for _, b := range backups {
		if !keep[b.name] && b.date.Format("2006-01-02") == yesterday {
			prevDay = b.name
			break
		}
	}
	if prevDay != "" {
		slot(prevDay)
	} else {
		slot(oldest.name)
	}

	// 1 weekly: newest backup >= 7 days old, else oldest
	slot(firstOlderThan(backups, now, 7*24*time.Hour, oldest.name))

	// 1 monthly: newest backup >= 30 days old, else oldest
	slot(firstOlderThan(backups, now, 30*24*time.Hour, oldest.name))

	return keep
}

func firstOlderThan(backups []backupEntry, now time.Time, age time.Duration, fallback string) string {
	for _, b := range backups {
		if now.Sub(b.date) >= age {
			return b.name
		}
	}
	return fallback
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func log(operation, message string) {
	logMsg("info", operation, message)
}

func logError(operation, message string) {
	logMsg("error", operation, message)
}

func logMsg(level, operation, message string) {
	entry := map[string]string{
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"level":     level,
		"operation": operation,
		"message":   message,
	}
	b, _ := json.Marshal(entry)
	if level == "error" || level == "fatal" {
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stdout, string(b))
	}
}

func timestamp() string {
	d := time.Now()
	return fmt.Sprintf("%04d%02d%02d_%02d%02d%02d",
		d.Year(), d.Month(), d.Day(), d.Hour(), d.Minute(), d.Second())
}

func run(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", cmd, msg)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

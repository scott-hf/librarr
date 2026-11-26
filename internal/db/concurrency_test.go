package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JeremiahM37/librarr/internal/models"
)

// TestNew_AppliesWALAndBusyTimeout pins the connection pragmas. The DSN used
// to pass mattn-style keys that modernc.org/sqlite ignores, so the database
// ran in rollback-journal mode with no busy timeout and concurrent writes
// failed with "database is locked".
//
// Lifted verbatim from upstream 9e37b2b.
func TestNew_AppliesWALAndBusyTimeout(t *testing.T) {
	d := newTestDB(t)
	var mode string
	if err := d.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := d.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 10000 {
		t.Fatalf("busy_timeout = %d, want 10000", timeout)
	}
	// The pragmas are per connection: exhaust the pool a little and re-check
	// on whatever connection answers.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var m string
			if err := d.db.QueryRow("PRAGMA journal_mode").Scan(&m); err != nil || m != "wal" {
				t.Errorf("pooled connection journal_mode = %q err=%v", m, err)
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentReadersAndWritersLoseNoWrites is the symptom rather than the
// setting: readers hold the library tables while several goroutines import.
// Every write must land. Cut down from upstream 9e37b2b's
// TestConcurrentReadersAndWriters to the library/activity APIs this fork has
// (the upstream version also exercises the wanted list, which was not taken).
func TestConcurrentReadersAndWritersLoseNoWrites(t *testing.T) {
	d := newTestDB(t)
	dir := t.TempDir()
	const writers, perWriter = 6, 40
	var readersWG, writersWG sync.WaitGroup
	errs := make(chan error, writers*perWriter*4)
	stop := make(chan struct{})

	for r := 0; r < 4; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := d.GetItems("", 50, 0); err != nil {
					errs <- fmt.Errorf("GetItems: %w", err)
				}
				if _, err := d.CountItems("ebook"); err != nil {
					errs <- fmt.Errorf("CountItems: %w", err)
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for i := 0; i < perWriter; i++ {
				title := fmt.Sprintf("Book %d-%d", w, i)
				path := filepath.Join(dir, title+".epub")
				if err := os.WriteFile(path, []byte(title), 0o644); err != nil {
					errs <- err
					continue
				}
				outcome, err := d.AddItemWithOutcome(&models.LibraryItem{Title: title, FilePath: path, FileFormat: "epub", MediaType: "ebook"})
				if err != nil {
					errs <- fmt.Errorf("AddItemWithOutcome: %w", err)
					continue
				}
				if err := d.LogEvent("test", title, "", &outcome.ID, ""); err != nil {
					errs <- fmt.Errorf("LogEvent: %w", err)
				}
			}
		}(w)
	}
	writersWG.Wait()
	close(stop)
	readersWG.Wait()
	close(errs)

	failures := 0
	for err := range errs {
		failures++
		if failures <= 5 {
			t.Error(err)
		}
	}
	if failures > 5 {
		t.Errorf("... and %d more errors", failures-5)
	}
	n, _ := d.CountItems("ebook")
	if n != writers*perWriter {
		t.Fatalf("expected %d library rows, got %d", writers*perWriter, n)
	}
}

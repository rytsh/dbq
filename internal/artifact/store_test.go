package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStoreCreateAndDownload(t *testing.T) {
	store, err := NewStore(t.TempDir(), time.Minute, 1024, 4096, 10, 2)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	metadata, err := store.Create(t.Context(), "../users", func(w io.Writer) error {
		_, err := io.WriteString(w, "INSERT INTO users VALUES (1);\n")

		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if metadata.Filename != "users.sql" || metadata.Size == 0 || metadata.SHA256 == "" {
		t.Fatalf("metadata = %+v", metadata)
	}

	req := httptest.NewRequest(http.MethodGet, "/exports/ignored", nil)
	req.SetPathValue("*", metadata.ID+"/users.sql")
	recorder := httptest.NewRecorder()
	store.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "INSERT INTO users VALUES (1);\n" {
		t.Errorf("body = %q", got)
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, "users.sql") {
		t.Errorf("content-disposition = %q", disposition)
	}
}

func TestStoreRejectsOversizeAndDoesNotPublish(t *testing.T) {
	store, err := NewStore(t.TempDir(), time.Minute, 3, 10, 10, 2)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.Create(t.Context(), "large.sql", func(w io.Writer) error {
		_, err := io.WriteString(w, "four")

		return err
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestStoreEnforcesFileCapacity(t *testing.T) {
	store, err := NewStore(t.TempDir(), time.Minute, 10, 20, 1, 1)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Create(t.Context(), "one.sql", func(w io.Writer) error {
		_, err := io.WriteString(w, "one")

		return err
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := store.Create(t.Context(), "two.sql", func(io.Writer) error { return nil }); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second create error = %v, want ErrCapacity", err)
	}
}

func TestCreateAfterCloseDoesNotDeadlock(t *testing.T) {
	store, err := NewStore(t.TempDir(), time.Minute, 10, 20, 1, 1)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.Create(context.Background(), "closed.sql", func(io.Writer) error { return nil })
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("create on closed store succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("create on closed store deadlocked")
	}
}

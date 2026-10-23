package repo

import (
	"errors"
	"fmt"
	"os"
	"path"
)

// WriteSummaryFile atomically publishes data as <repo>/summary and removes any
// stale summary.sig, mirroring ostree_repo_regenerate_metadata(): the summary
// is staged below tmp/ and renamed into place so clients never observe a
// partially written file.
func (r *Repo) WriteSummaryFile(data []byte) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return err
	}
	tmp := path.Join("tmp", "summary-"+randomHex(8))
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return fmt.Errorf("create summary: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		r.root.Remove(tmp)
		return fmt.Errorf("write summary: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		r.root.Remove(tmp)
		return fmt.Errorf("write summary: %w", err)
	}
	if err := f.Close(); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("write summary: %w", err)
	}
	if err := r.root.Rename(tmp, "summary"); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("publish summary: %w", err)
	}
	// libostree deletes an existing signature when publishing an unsigned
	// summary so the pair never mismatches; a missing one is fine.
	if err := r.root.Remove("summary.sig"); err != nil && !isMissingPath(err) {
		return fmt.Errorf("remove summary.sig: %w", err)
	}
	return nil
}

// ReadSummaryFile returns the raw <repo>/summary bytes.
func (r *Repo) ReadSummaryFile() ([]byte, error) {
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	return r.readRootFile("summary")
}

// ReadSummarySignature returns the raw <repo>/summary.sig bytes.
func (r *Repo) ReadSummarySignature() ([]byte, error) {
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	data, err := r.readRootFile("summary.sig")
	if err != nil {
		if isMissingPath(err) {
			return nil, ErrObjectNotFound
		}
		return nil, err
	}
	return data, nil
}

// WriteSummaryAndSignature publishes data and its detached signature
// together, staging both below tmp/ and renaming them into place like
// ostree_repo_regenerate_metadata().  The summary's timestamps are synced to
// the signature file so HTTP clients expire both at the same time.
func (r *Repo) WriteSummaryAndSignature(data, sig []byte) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return err
	}
	base := "summary-" + randomHex(8)
	tmpSummary := path.Join("tmp", base)
	tmpSig := path.Join("tmp", base+".sig")
	writeFile := func(rel string, contents []byte) error {
		f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			return fmt.Errorf("create %s: %w", rel, err)
		}
		if _, err := f.Write(contents); err != nil {
			f.Close()
			r.root.Remove(rel)
			return fmt.Errorf("write %s: %w", rel, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			r.root.Remove(rel)
			return fmt.Errorf("write %s: %w", rel, err)
		}
		if err := f.Close(); err != nil {
			r.root.Remove(rel)
			return fmt.Errorf("write %s: %w", rel, err)
		}
		return nil
	}
	if err := writeFile(tmpSummary, data); err != nil {
		return err
	}
	if err := writeFile(tmpSig, sig); err != nil {
		r.root.Remove(tmpSummary)
		return err
	}
	// Sync summary mtime to summary.sig before publishing.
	if st, err := r.root.Stat(tmpSig); err == nil {
		if err := os.Chtimes(r.rootAbsPath(tmpSummary), st.ModTime(), st.ModTime()); err != nil {
			r.root.Remove(tmpSummary)
			r.root.Remove(tmpSig)
			return fmt.Errorf("Unable to change summary timestamps: %w", err)
		}
	}
	if err := r.root.Rename(tmpSummary, "summary"); err != nil {
		r.root.Remove(tmpSummary)
		r.root.Remove(tmpSig)
		return fmt.Errorf("publish summary: %w", err)
	}
	if err := r.root.Rename(tmpSig, "summary.sig"); err != nil {
		r.root.Remove(tmpSig)
		return fmt.Errorf("publish summary signature: %w", err)
	}
	return nil
}

package pull

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// metalinkDocument is the subset of Metalink 3.0 understood by libostree.
type metalinkDocument struct {
	XMLName xml.Name       `xml:"metalink"`
	Files   []metalinkFile `xml:"files>file"`
}

type metalinkFile struct {
	Name         string                `xml:"name,attr"`
	Size         uint64                `xml:"size"`
	Verification *metalinkVerification `xml:"verification"`
	Resources    *metalinkResources    `xml:"resources"`
}

type metalinkVerification struct {
	Hashes []metalinkHash `xml:"hash"`
}

type metalinkHash struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type metalinkResources struct {
	URLs []metalinkURL `xml:"url"`
}

type metalinkURL struct {
	Protocol string `xml:"protocol,attr"`
	Value    string `xml:",chardata"`
}

// resolveMetalink downloads and parses a metalink document, then fetches the
// requested file ("summary") from the highest-preference usable URL.
//
// It returns the base URL of the successful target (used as the metadata
// mirror for the rest of the pull) along with the file contents.
func resolveMetalink(f *fetcher, metalinkURL string) (string, []byte, error) {
	body, err := f.fetchAbsolute(metalinkURL)
	if err != nil {
		return "", nil, err
	}

	var doc metalinkDocument
	if err := xml.Unmarshal(body, &doc); err != nil {
		return "", nil, fmt.Errorf("Document must begin with an element")
	}

	if len(doc.Files) == 0 {
		return "", nil, fmt.Errorf("No <file> element found")
	}
	var file *metalinkFile
	for i := range doc.Files {
		if doc.Files[i].Name == "summary" {
			file = &doc.Files[i]
			break
		}
	}
	if file == nil {
		return "", nil, fmt.Errorf("No <file name='summary'> found")
	}

	// libostree validates size/verification when it reaches <resources>.
	if file.Resources != nil {
		if file.Size == 0 {
			return "", nil, fmt.Errorf("No <size> element found or it is zero")
		}
		if !hasKnownMetalinkHash(file.Verification) {
			return "", nil, fmt.Errorf("No <verification> element with known <hash type=> found")
		}
	}

	sha256sum, sha512sum := metalinkHashes(file.Verification)
	if sha256sum == "" && sha512sum == "" {
		return "", nil, fmt.Errorf("No <verification> hash for sha256 or sha512 found")
	}
	if sha256sum != "" && !validHexDigest(sha256sum, 64) {
		return "", nil, fmt.Errorf("Invalid hash digest for sha256")
	}
	if sha512sum != "" && !validHexDigest(sha512sum, 128) {
		return "", nil, fmt.Errorf("Invalid hash digest for sha512")
	}

	var urls []string
	if file.Resources != nil {
		for _, resource := range file.Resources.URLs {
			if resource.Protocol != "http" && resource.Protocol != "https" {
				continue
			}
			candidate := strings.TrimSpace(resource.Value)
			if candidate == "" {
				continue
			}
			urls = append(urls, candidate)
		}
	}
	if len(urls) == 0 {
		return "", nil, fmt.Errorf("No <url method='http'> elements found")
	}

	var lastErr error
	for _, target := range urls {
		data, err := f.fetchAbsolute(target)
		if err != nil {
			lastErr = err
			continue
		}
		if uint64(len(data)) != file.Size {
			lastErr = fmt.Errorf("Expected size is %d bytes but content is %d bytes",
				file.Size, len(data))
			continue
		}
		if sha512sum != "" {
			sum := sha512.Sum512(data)
			actual := hex.EncodeToString(sum[:])
			if !strings.EqualFold(actual, sha512sum) {
				lastErr = fmt.Errorf("Expected checksum is %s but actual is %s", sha512sum, actual)
				continue
			}
		} else {
			sum := sha256.Sum256(data)
			actual := hex.EncodeToString(sum[:])
			if !strings.EqualFold(actual, sha256sum) {
				lastErr = fmt.Errorf("Expected checksum is %s but actual is %s", sha256sum, actual)
				continue
			}
		}
		base, err := metalinkTargetBase(target)
		if err != nil {
			lastErr = err
			continue
		}
		return base, data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("No usable metalink targets")
	}
	return "", nil, fmt.Errorf("Exhausted %d metalink targets, last error: %w", len(urls), lastErr)
}

func hasKnownMetalinkHash(v *metalinkVerification) bool {
	if v == nil {
		return false
	}
	for _, h := range v.Hashes {
		if h.Type == "sha256" || h.Type == "sha512" {
			return true
		}
	}
	return false
}

func metalinkHashes(v *metalinkVerification) (sha256sum, sha512sum string) {
	if v == nil {
		return "", ""
	}
	for _, h := range v.Hashes {
		value := strings.TrimSpace(h.Value)
		switch h.Type {
		case "sha256":
			sha256sum = value
		case "sha512":
			sha512sum = value
		}
	}
	return sha256sum, sha512sum
}

// validHexDigest matches valid_hex_checksum(): lowercase hex only and the
// exact expected length.
func validHexDigest(s string, expected int) bool {
	if len(s) != expected {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// metalinkTargetBase returns the directory containing the chosen URL, which
// becomes the metadata mirror for the remainder of the pull.
func metalinkTargetBase(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	dir := path.Dir(u.Path)
	if dir == "." {
		dir = "/"
	}
	u.Path = dir
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/"), nil
}

package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"gpustack.ai/gpustack/pkg/modelmanager/peer"
)

// readListing reads the published tree's transfer listing: the exact bytes
// the publish stored beside the marker, decoded under the strict bounds. A
// tree published before listings were stored carries none and is not a
// listing source; a listing that is malformed, oversized, or carries one
// unknown or repeated field is refused whole.
func readListing(path string) (peer.Listing, error) {
	var listing peer.Listing
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return listing, ErrListingUnavailable
	case err != nil:
		return listing, err
	case !info.Mode().IsRegular():
		return listing, fmt.Errorf("%w: the listing is not a regular file", ErrListingInvalid)
	case info.Size() > MaxListingBytes:
		return listing, fmt.Errorf("%w: the listing holds %d bytes, above the %d bound",
			ErrListingInvalid, info.Size(), MaxListingBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return listing, err
	}
	if int64(len(raw)) > MaxListingBytes {
		return listing, fmt.Errorf("%w: the listing holds %d bytes, above the %d bound",
			ErrListingInvalid, len(raw), MaxListingBytes)
	}
	if err := decodeListing(raw, &listing); err != nil {
		return listing, err
	}

	return listing, nil
}

// decodeListing decodes a listing's JSON the strict way: one object holding
// digest and entries, each key at most once, no unknown field, no trailing
// token, and entry counts inside the bound. encoding/json's own decoder
// silently keeps the last of repeated keys and ignores unknown ones, so the
// object is walked token by token instead.
func decodeListing(raw []byte, listing *peer.Listing) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: the listing is empty", ErrListingInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := expectListingDelim(dec, json.Delim('{')); err != nil {
		return err
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := nextListingKey(dec)
		if err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("%w: the listing repeats the field %q", ErrListingInvalid, key)
		}
		seen[key] = true
		switch key {
		case "digest":
			if err := dec.Decode(&listing.Digest); err != nil {
				return fmt.Errorf("%w: the listing digest is malformed: %w", ErrListingInvalid, err)
			}
		case "entries":
			if err := decodeListingEntries(dec, &listing.Entries); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: the listing carries the unknown field %q", ErrListingInvalid, key)
		}
	}
	if err := expectListingDelim(dec, json.Delim('}')); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON tokens after the listing object", ErrListingInvalid)
	}
	if !seen["digest"] || !seen["entries"] {
		return fmt.Errorf("%w: the listing lacks its digest or entries", ErrListingInvalid)
	}

	return nil
}

// decodeListingEntries decodes the listing's files, one object per file, each
// carrying path, size and digest and nothing else.
func decodeListingEntries(dec *json.Decoder, entries *[]peer.ManifestFile) error {
	if err := expectListingDelim(dec, json.Delim('[')); err != nil {
		return err
	}
	for dec.More() {
		if len(*entries) >= MaxListingEntries {
			return fmt.Errorf("%w: the listing holds more than %d files", ErrListingInvalid, MaxListingEntries)
		}
		if err := expectListingDelim(dec, json.Delim('{')); err != nil {
			return err
		}
		var file peer.ManifestFile
		seen := map[string]bool{}
		for dec.More() {
			key, err := nextListingKey(dec)
			if err != nil {
				return err
			}
			if seen[key] {
				return fmt.Errorf("%w: a listed file repeats the field %q", ErrListingInvalid, key)
			}
			seen[key] = true
			switch key {
			case "path":
				if err := dec.Decode(&file.Path); err != nil {
					return fmt.Errorf("%w: a listed file's path is malformed: %w", ErrListingInvalid, err)
				}
			case "size":
				if err := dec.Decode(&file.Size); err != nil {
					return fmt.Errorf("%w: a listed file's size is malformed: %w", ErrListingInvalid, err)
				}
			case "digest":
				if err := dec.Decode(&file.Digest); err != nil {
					return fmt.Errorf("%w: a listed file's digest is malformed: %w", ErrListingInvalid, err)
				}
			default:
				return fmt.Errorf("%w: a listed file carries the unknown field %q", ErrListingInvalid, key)
			}
		}
		if err := expectListingDelim(dec, json.Delim('}')); err != nil {
			return err
		}
		if !seen["path"] || !seen["size"] || !seen["digest"] {
			return fmt.Errorf("%w: a listed file lacks its path, size or digest", ErrListingInvalid)
		}
		*entries = append(*entries, file)
	}

	return expectListingDelim(dec, json.Delim(']'))
}

// nextListingKey reads the next object key, which must be a string.
func nextListingKey(dec *json.Decoder) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("%w: the listing is not valid JSON: %w", ErrListingInvalid, err)
	}
	key, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("%w: a listing key is not a string", ErrListingInvalid)
	}

	return key, nil
}

// expectListingDelim consumes the next token and refuses unless it is the
// delimiter wanted.
func expectListingDelim(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: the listing is not valid JSON: %w", ErrListingInvalid, err)
	}
	if token != want {
		return fmt.Errorf("%w: the listing is not shaped like a listing", ErrListingInvalid)
	}

	return nil
}

package hfget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Custom error types for specific, non-transient HTTP errors.
var (
	ErrAuthentication = errors.New("authentication failed (401): check your token")
	ErrForbidden      = errors.New("forbidden (403): you may need to accept the repository's terms on the Hugging Face website")
	ErrNotFound       = errors.New("not found (404): check the repository name and branch")

	baseURL                = "https://huggingface.co"
)

const (
	jsonModelsInfoURL      = "/api/models/%s?revision=%s"
	jsonDatasetsInfoURL    = "/api/datasets/%s?revision=%s"
	jsonModelsFileTreeURL  = "/api/models/%s/tree/%s"
	jsonDatasetFileTreeURL = "/api/datasets/%s/tree/%s"
	rawModelFileURL        = "/%s/raw/%s/%s"
	rawDatasetFileURL      = "/datasets/%s/raw/%s/%s"
	lfsModelResolverURL    = "/%s/resolve/%s/%s"
	lfsDatasetResolverURL  = "/datasets/%s/resolve/%s/%s"
)

// RepoInfo holds metadata about a Hugging Face repository.
type RepoInfo struct {
	ID           string
	LastModified time.Time
	Siblings     []HFFile // The list of files/folders in the root directory
}

// UnmarshalJSON for RepoInfo handles custom parsing.
func (r *RepoInfo) UnmarshalJSON(data []byte) error {
	type Alias RepoInfo
	aux := &struct {
		ID           string    `json:"id"`
		LastModified time.Time `json:"lastModified"`
		Siblings     []struct {
			Rfilename string `json:"rfilename"`
		} `json:"siblings"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.ID = aux.ID
	r.LastModified = aux.LastModified
	r.Siblings = make([]HFFile, len(aux.Siblings))
	for i, s := range aux.Siblings {
		r.Siblings[i] = HFFile{Path: s.Rfilename, Type: "file"}
	}
	return nil
}

// HFFile represents a file or directory node from the Hugging Face API.
type HFFile struct {
	Type string `json:"type"`
	Oid  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  HFLFS  `json:"lfs"`
}

// HFLFS contains LFS metadata for a file.
type HFLFS struct {
	IsLFS       bool   `json:"-"`
	Oid         string `json:"oid"`
	Size        int64  `json:"size"`
	PointerSize int64  `json:"pointerSize"`
}

// UnmarshalJSON for HFFile handles custom logic for determining if a file is LFS.
func (f *HFFile) UnmarshalJSON(data []byte) error {
	type Alias HFFile
	aux := &struct {
		*Alias
	}{Alias: (*Alias)(f)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if f.LFS.Oid != "" {
		f.LFS.IsLFS = true
		f.Size = f.LFS.Size
	}
	return nil
}

// fetchRepoInfo fetches the main metadata and root file list for a repository.
func (d *Downloader) fetchRepoInfo(ctx context.Context) (*RepoInfo, error) {
	var urlFormat string
	if d.isDataset {
		urlFormat = jsonDatasetsInfoURL
	} else {
		urlFormat = jsonModelsInfoURL
	}
	apiURL := baseURL + fmt.Sprintf(urlFormat, d.repoName, url.QueryEscape(d.branch))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create repo info request for %s: %w", apiURL, err)
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed for %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if err := handleAPIError(resp, apiURL); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from %s: %w", apiURL, err)
	}

	var info RepoInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal repo info from %s: %w", apiURL, err)
	}
	
	rootTree, err := d.fetchTree(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch root tree to complement repo info: %w", err)
	}
	info.Siblings = rootTree
	return &info, nil
}

// fetchTree calls the Hugging Face API to get the file list for a directory.
func (d *Downloader) fetchTree(ctx context.Context, folderPath string) ([]HFFile, error) {
	apiURL := d.buildTreeURL(folderPath)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create tree request for %s: %w", apiURL, err)
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed for %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if err := handleAPIError(resp, apiURL); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from %s: %w", apiURL, err)
	}

	var files []HFFile
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON from %s: %w", apiURL, err)
	}
	return files, nil
}

// resolveDownloadURL gets the final, redirect S3/Cloudfront URL for a file.
func (d *Downloader) resolveDownloadURL(ctx context.Context, file HFFile) (string, error) {
	resolverURL := d.buildResolverURL(file.Path, file.LFS.IsLFS)
	req, err := http.NewRequestWithContext(ctx, "GET", resolverURL, nil)
	if err != nil {
		return "", err
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := handleAPIError(resp, resolverURL); err != nil {
		return "", err
	}

	if file.LFS.IsLFS {
		if location := resp.Header.Get("Location"); location != "" {
			return location, nil
		}
		return "", fmt.Errorf("no redirect location found for LFS file: %s", file.Path)
	}

	return resolverURL, nil
}

// --- ADDED BACK MISSING FUNCTION ---
// handleAPIError checks the HTTP response for common errors and returns a typed error.
func handleAPIError(resp *http.Response, url string) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusFound, http.StatusTemporaryRedirect:
		return nil
	case http.StatusUnauthorized:
		return ErrAuthentication
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, url)
	}
}


func (d *Downloader) buildTreeURL(folderPath string) string {
	var urlFormat string
	if d.isDataset {
		urlFormat = jsonDatasetFileTreeURL
	} else {
		urlFormat = jsonModelsFileTreeURL
	}
	baseAPIPath := fmt.Sprintf(urlFormat, d.repoName, url.QueryEscape(d.branch))
	fullURL := baseURL + baseAPIPath
	if folderPath != "" {
		fullURL = fullURL + "/" + url.PathEscape(folderPath)
	}
	return fullURL
}

func (d *Downloader) buildResolverURL(filePath string, isLFS bool) string {
	var urlFormat string
	if isLFS {
		if d.isDataset {
			urlFormat = lfsDatasetResolverURL
		} else {
			urlFormat = lfsModelResolverURL
		}
	} else {
		if d.isDataset {
			urlFormat = rawDatasetFileURL
		} else {
			urlFormat = rawModelFileURL
		}
	}
	return baseURL + fmt.Sprintf(urlFormat, d.repoName, url.QueryEscape(d.branch), filePath)
}

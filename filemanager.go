//go:generate go get github.com/jteeuwen/go-bindata
//go:generate go install github.com/jteeuwen/go-bindata/go-bindata
//go:generate go-bindata -pkg assets -o assets/assets.go assets/source/...

// Package filemanager provides middleware for managing files in a directory
// when directory path is requested instead of a specific file. Based on browse
// middleware.
package filemanager

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hacdias/caddy-filemanager/assets"
	"github.com/mholt/caddy/caddyhttp/httpserver"
	"github.com/mholt/caddy/caddyhttp/staticfiles"
)

// FileManager is an http.Handler that can show a file listing when
// directories in the given paths are specified.
type FileManager struct {
	Next          httpserver.Handler
	Configs       []Config
	IgnoreIndexes bool
}

// Config is a configuration for browsing in a particular path.
type Config struct {
	PathScope string
	Root      http.FileSystem
	Variables interface{}
	Template  *template.Template
}

// A Listing is the context used to fill out a template.
type Listing struct {
	// The name of the directory (the last element of the path)
	Name string

	// The full path of the request
	Path string

	// Whether the parent directory is browsable
	CanGoUp bool

	// The items (files and folders) in the path
	Items []FileInfo

	// The number of directories in the listing
	NumDirs int

	// The number of files (items that aren't directories) in the listing
	NumFiles int

	// Which sorting order is used
	Sort string

	// And which order
	Order string

	// If ≠0 then Items have been limited to that many elements
	ItemsLimitedTo int

	// Optional custom variables for use in browse templates
	User interface{}

	httpserver.Context
}

// BreadcrumbMap returns l.Path where every element is a map
// of URLs and path segment names.
func (l Listing) BreadcrumbMap() map[string]string {
	result := map[string]string{}

	if len(l.Path) == 0 {
		return result
	}

	// skip trailing slash
	lpath := l.Path
	if lpath[len(lpath)-1] == '/' {
		lpath = lpath[:len(lpath)-1]
	}

	parts := strings.Split(lpath, "/")
	for i, part := range parts {
		if i == 0 && part == "" {
			// Leading slash (root)
			result["/"] = "/"
			continue
		}
		result[strings.Join(parts[:i+1], "/")] = part
	}

	return result
}

// FileInfo is the info about a particular file or directory
type FileInfo struct {
	IsDir   bool
	Name    string
	Size    int64
	URL     string
	ModTime time.Time
	Mode    os.FileMode
}

// HumanSize returns the size of the file as a human-readable string
// in IEC format (i.e. power of 2 or base 1024).
func (fi FileInfo) HumanSize() string {
	return humanize.IBytes(uint64(fi.Size))
}

// HumanModTime returns the modified time of the file as a human-readable string.
func (fi FileInfo) HumanModTime(format string) string {
	return fi.ModTime.Format(format)
}

func directoryListing(files []os.FileInfo, canGoUp bool, urlPath string) (Listing, bool) {
	var (
		fileinfos           []FileInfo
		dirCount, fileCount int
		hasIndexFile        bool
	)

	for _, f := range files {
		name := f.Name()

		for _, indexName := range staticfiles.IndexPages {
			if name == indexName {
				hasIndexFile = true
				break
			}
		}

		if f.IsDir() {
			name += "/"
			dirCount++
		} else {
			fileCount++
		}

		url := url.URL{Path: "./" + name} // prepend with "./" to fix paths with ':' in the name

		fileinfos = append(fileinfos, FileInfo{
			IsDir:   f.IsDir(),
			Name:    f.Name(),
			Size:    f.Size(),
			URL:     url.String(),
			ModTime: f.ModTime().UTC(),
			Mode:    f.Mode(),
		})
	}

	return Listing{
		Name:     path.Base(urlPath),
		Path:     urlPath,
		CanGoUp:  canGoUp,
		Items:    fileinfos,
		NumDirs:  dirCount,
		NumFiles: fileCount,
	}, hasIndexFile
}

// ServeHTTP determines if the request is for this plugin, and if all prerequisites are met.
// If so, control is handed over to ServeListing.
func (f FileManager) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	var bc *Config
	// See if there's a browse configuration to match the path
	for i := range f.Configs {
		if httpserver.Path(r.URL.Path).Matches(f.Configs[i].PathScope) {
			bc = &f.Configs[i]
			goto inScope
		}
	}
	return f.Next.ServeHTTP(w, r)
inScope:

	// Browse works on existing directories; delegate everything else
	requestedFilepath, err := bc.Root.Open(r.URL.Path)
	if err != nil {
		switch {
		case os.IsPermission(err):
			return http.StatusForbidden, err
		case os.IsExist(err):
			return http.StatusNotFound, err
		default:
			return f.Next.ServeHTTP(w, r)
		}
	}
	defer requestedFilepath.Close()

	info, err := requestedFilepath.Stat()
	if err != nil {
		switch {
		case os.IsPermission(err):
			return http.StatusForbidden, err
		case os.IsExist(err):
			return http.StatusGone, err
		default:
			return f.Next.ServeHTTP(w, r)
		}
	}
	if !info.IsDir() {
		return f.Next.ServeHTTP(w, r)
	}

	// Do not reply to anything else because it might be nonsensical
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// proceed, noop
	case "PROPFIND", http.MethodOptions:
		return http.StatusNotImplemented, nil
	default:
		return f.Next.ServeHTTP(w, r)
	}

	// Browsing navigation gets messed up if browsing a directory
	// that doesn't end in "/" (which it should, anyway)
	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
		return 0, nil
	}

	return f.ServeListing(w, r, requestedFilepath, bc)
}

func (f FileManager) loadDirectoryContents(requestedFilepath http.File, urlPath string) (*Listing, bool, error) {
	files, err := requestedFilepath.Readdir(-1)
	if err != nil {
		return nil, false, err
	}

	// Determine if user can browse up another folder
	var canGoUp bool
	curPathDir := path.Dir(strings.TrimSuffix(urlPath, "/"))
	for _, other := range f.Configs {
		if strings.HasPrefix(curPathDir, other.PathScope) {
			canGoUp = true
			break
		}
	}

	// Assemble listing of directory contents
	listing, hasIndex := directoryListing(files, canGoUp, urlPath)

	return &listing, hasIndex, nil
}

// ServeListing returns a formatted view of 'requestedFilepath' contents'.
func (f FileManager) ServeListing(w http.ResponseWriter, r *http.Request, requestedFilepath http.File, bc *Config) (int, error) {
	listing, containsIndex, err := f.loadDirectoryContents(requestedFilepath, r.URL.Path)
	if err != nil {
		switch {
		case os.IsPermission(err):
			return http.StatusForbidden, err
		case os.IsExist(err):
			return http.StatusGone, err
		default:
			return http.StatusInternalServerError, err
		}
	}
	if containsIndex && !f.IgnoreIndexes { // directory isn't browsable
		return f.Next.ServeHTTP(w, r)
	}
	listing.Context = httpserver.Context{
		Root: bc.Root,
		Req:  r,
		URL:  r.URL,
	}
	listing.User = bc.Variables

	// Copy the query values into the Listing struct
	var limit int
	listing.Sort, listing.Order, limit, err = f.handleSortOrder(w, r, bc.PathScope)
	if err != nil {
		return http.StatusBadRequest, err
	}

	listing.applySort()

	if limit > 0 && limit <= len(listing.Items) {
		listing.Items = listing.Items[:limit]
		listing.ItemsLimitedTo = limit
	}

	var buf *bytes.Buffer
	acceptHeader := strings.ToLower(strings.Join(r.Header["Accept"], ","))
	switch {
	case strings.Contains(acceptHeader, "application/json"):
		if buf, err = f.formatAsJSON(listing, bc); err != nil {
			return http.StatusInternalServerError, err
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

	default: // There's no 'application/json' in the 'Accept' header; browse normally
		if buf, err = f.formatAsHTML(listing, bc); err != nil {
			return http.StatusInternalServerError, err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

	}

	buf.WriteTo(w)

	return http.StatusOK, nil
}

// serveAssets handles the /{admin}/assets requests
func serveAssets(w http.ResponseWriter, r *http.Request) (int, error) {
	filename := strings.Replace(r.URL.Path, assetsURL, "", 1)
	file, err := assets.Asset(filename)

	if err != nil {
		return 404, nil
	}

	// Get the file extension ant its mime type
	extension := filepath.Ext(filename)
	mime := mime.TypeByExtension(extension)

	// Write the header with the Content-Type and write the file
	// content to the buffer
	w.Header().Set("Content-Type", mime)
	w.Write(file)
	return 200, nil
}

func (f FileManager) formatAsJSON(listing *Listing, bc *Config) (*bytes.Buffer, error) {
	marsh, err := json.Marshal(listing.Items)
	if err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	_, err = buf.Write(marsh)
	return buf, err
}

func (f FileManager) formatAsHTML(listing *Listing, bc *Config) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	err := bc.Template.Execute(buf, listing)
	return buf, err
}

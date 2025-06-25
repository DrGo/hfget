package hfget

import (
	"io"
	"time"
)

// WithAuthToken sets the Hugging Face auth token.
func WithAuthToken(token string) Option {
	return func(d *Downloader) {
		if token != "" {
			d.authToken = token
		}
	}
}

// WithConnections sets the number of concurrent connections for multi-threaded downloads.
func WithConnections(n int) Option {
	return func(d *Downloader) {
		if n > 0 {
			d.numConnections = n
		}
	}
}

// WithBranch sets the repository branch to download from.
func WithBranch(branch string) Option {
	return func(d *Downloader) {
		if branch != "" {
			d.branch = branch
		}
	}
}

// WithDestination sets the base directory for downloads.
func WithDestination(dest string) Option {
	return func(d *Downloader) {
		if dest != "" {
			d.destinationBasePath = dest
		}
	}
}

// Add these functions to downloader.go

// WithInclude sets the include glob patterns for filtering files.
func WithInclude(patterns ...string) Option {
	return func(d *Downloader) {
		d.includePatterns = patterns
	}
}

// WithExclude sets the exclude glob patterns for filtering files.
func WithExclude(patterns ...string) Option {
	return func(d *Downloader) {
		d.excludePatterns = patterns
	}
}

// WithNumConnections sets the number of parallel connections for multi-threaded downloads.
func WithNumConnections(n int) Option {
	return func(d *Downloader) {
		if n > 0 {
			d.numConnections = n
		}
	}
}

// WithProgress sets the channel for progress reporting.
func WithProgress(ch chan<- Progress) Option {
	return func(d *Downloader) {
		d.Progress = ch
	}
}

// AsDataset specifies that the repository is a dataset.
func AsDataset() Option {
	return func(d *Downloader) {
		d.isDataset = true
	}
}

// WithIncludePatterns sets glob patterns for files to include.
func WithIncludePatterns(patterns []string) Option {
	return func(d *Downloader) {
		d.includePatterns = patterns
	}
}

// WithExcludePatterns sets glob patterns for files to exclude.
func WithExcludePatterns(patterns []string) Option {
	return func(d *Downloader) {
		d.excludePatterns = patterns
	}
}
// WithProgressChannel sets a channel to receive progress updates.
func WithProgressChannel(p chan<- Progress) Option {
	return func(d *Downloader) {
		d.Progress = p
	}
}

// SkipSHACheck disables SHA256 checksum verification for LFS files.
func SkipSHACheck() Option {
	return func(d *Downloader) {
		d.skipSHA = true
	}
}

// WithForceRedownload bypasses local file checks and downloads all files.
func WithForceRedownload() Option {
	return func(d *Downloader) {
		d.forceRedownload = true
	}
}

// WithTreeStructure enables saving to a nested directory structure (e.g., org/model).
func WithTreeStructure() Option {
	return func(d *Downloader) {
		d.useTreeStructure = true
	}
}


// WithVerboseOutput sets an io.Writer for verbose logging.
func WithVerboseOutput(w io.Writer) Option {
	return func(d *Downloader) {
		d.setLogger(w)
	}
}

// WithTimeout sets the timeout for all HTTP requests.
func WithTimeout(timeout time.Duration) Option {
	return func(d *Downloader) {
		if timeout > 0 {
			d.client.Timeout = timeout
		}
	}
}

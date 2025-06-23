package hfget

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

// AsDataset specifies that the repository is a dataset.
func AsDataset() Option {
	return func(d *Downloader) {
		d.isDataset = true
	}
}

// WithFilter specifies which files to include based on substrings.
func WithFilter(filter []string) Option {
	return func(d *Downloader) {
		d.filter = filter
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

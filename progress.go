package hfget

// ProgressState defines the current status of a file download.
type ProgressState int

const (
	// ProgressStateDownloading indicates that the file is actively being downloaded.
	ProgressStateDownloading ProgressState = iota
	// ProgressStateComplete indicates that the file has been successfully downloaded and verified.
	ProgressStateComplete
	// ProgressStateSkipped indicates that the file download was skipped (already exists or filtered).
	ProgressStateSkipped
)

// Progress holds the state of a file download operation, designed to be sent over a channel.
type Progress struct {
	Filepath    string
	TotalSize   int64
	CurrentSize int64
	State       ProgressState
	Message     string
}

package hfget

// ProgressState defines the current status of a file operation.
type ProgressState int

const (
	// ProgressStateDownloading indicates that the file is actively being downloaded.
	ProgressStateDownloading ProgressState = iota
	// ProgressStateVerifying indicates that a local file's checksum is being calculated.
	ProgressStateVerifying
	// ProgressStateComplete indicates that the file download is finished, pending verification.
	ProgressStateComplete
	// ProgressStateVerified indicates that the file has been successfully verified.
	ProgressStateVerified
	// ProgressStateSkipped indicates that the file download was skipped.
	ProgressStateSkipped
)

// Progress holds the state of a file operation, designed to be sent over a channel.
type Progress struct {
	Filepath    string
	TotalSize   int64
	CurrentSize int64
	State       ProgressState
	Message     string
}

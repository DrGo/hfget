# hfget - A Robust Hugging Face Downloader

`hfget` is a lightweight, command-line utility for downloading models and datasets from Hugging Face. It is designed to be fast, reliable, and script-friendly, with features like concurrent downloads, file integrity verification, and advanced filtering.

## Features

* **Concurrent Downloads:** Utilizes multiple connections to download large files in parallel, significantly speeding up the process.
* **Integrity Verification:** Automatically verifies downloaded files against their expected size and SHA256 checksum (for LFS files) to ensure they are not corrupted.
* **Intelligent Syncing:** Only downloads files that are missing or have failed local verification, saving time and bandwidth.
* **Advanced Filtering:** Include or exclude specific files from a repository using glob patterns.
* **Robust Error Handling:** Features an idle timeout to prevent freezes on stalled connections and retries on transient network errors. A single file failure will not stop the entire download job.
* **Accurate Progress Display:** Provides smooth, accurate progress bars for both the initial file analysis and the download phases.
* **Interactive & Scriptable:** Provides an interactive summary and confirmation prompt for manual use, which is automatically bypassed when not run in a terminal or when using the `--force` flag.
* **Verbose Logging:** An optional `--verbose` flag provides detailed diagnostic output for troubleshooting.
* **Lightweight & Portable:** The compiled binary is self-contained and works on major platforms with no runtime dependencies. It has minimal external Go library dependencies.

## Installation

### Via Pre-compiled Binary (Recommended)

You can download the latest pre-compiled binary for your operating system (Linux, macOS, Windows) from the [**Releases**](https://github.com/drgo/hfget/releases) page on GitHub. This is the simplest way to get started.

### Via `go install`

If you have a working Go environment, you can also install `hfget` with a single command:

```sh
go install [github.com/drgo/hfget/cmd/hfget@latest](https://github.com/drgo/hfget/cmd/hfget@latest)
```

This will download the source, compile it, and place the `hfget` binary in your `$GOPATH/bin` directory. Make sure this directory is in your system's `PATH`.

## Usage

### Basic Syntax

The tool is invoked by providing the repository name as an argument, followed by any optional flags.

```sh
hfget [OPTIONS] REPOSITORY_NAME
```

### Examples

**1. Download a Model**

This will download the `nanollama` model from the user `imdatta0` into a directory named `imdatta0_nanollama` in the current folder.

```sh
hfget imdatta0/nanollama
```

**2. Download to a Specific Directory**

Using the `-d` flag specifies the destination directory. (The long form is `--dest`).

```sh
# This will save files to the 'my_models' directory
hfget -d ./my_models imdatta0/nanollama
```

**3. Download a Dataset**

To download a dataset, you must use the `--dataset` flag.

```sh
hfget --dataset squad
```

**4. Download with Filtering**

Use the `--include` and `--exclude` flags with comma-separated glob patterns to control which files are downloaded.

```sh
# Download only the Q4 and Q5 quantizations from a model
hfget imdatta0/nanollama --include "*.Q4_K_M.gguf,*.Q5_K_M.gguf"

# Download everything EXCEPT the safetensors files
hfget imdatta0/nanollama --exclude "*.safetensors"
```

**5. Force a Re-download**

To re-download all files from a repository, regardless of their local state, use the `-f` flag. This will also skip all interactive prompts.

```sh
hfget -f imdatta0/nanollama
```

### Command-Line Flags

Flags can also be set via environment variables (e.g., setting `HFGET_TOKEN` instead of using the `-t` flag).

| Flag             | Shorthand | Environment Variable           | Description                                                 | Default |
| :--------------- | :-------- | :----------------------------- | :---------------------------------------------------------- | :------ |
| `--dataset`      |           |                                | Specify that the repository is a dataset.                   | `false` |
| `--branch`       | `-b`      | `HFGET_BRANCH`                 | The repository branch to download from.                     | `"main"`  |
| `--dest`         | `-d`      | `HFGET_DEST`                   | The local directory where files will be saved.              | `"./"`    |
|                  | `-c`      | `HFGET_CONCURRENT_CONNECTIONS` | Number of concurrent connections for downloading.           | `5`       |
| `--token`        | `-t`      | `HFGET_TOKEN`                  | Your Hugging Face auth token.                               | `""`      |
| `--skip-checksum`|           | `HFGET_SKIP_CHECKSUM`          | Skip SHA256 checksum verification.                          | `false`   |
| `--tree`         |           |                                | Use nested tree structure for output directory.             | `false`   |
| `--include`      |           |                                | Comma-separated glob patterns for files to include.         | `""`      |
| `--exclude`      |           |                                | Comma-separated glob patterns for files to exclude.         | `""`      |
| `--max-retries`  |           |                                | Maximum retries on transient network errors.                | `3`       |
| `--retry-interval` |         |                                | The time to wait between retries.                           | `5s`      |
| `--quiet`        | `-q`      |                                | Suppress interactive progress and prompts.                  | `false`   |
| `--force`        | `-f`      |                                | Force re-download of all files (implies `--quiet`).         | `false`   |
| `--verbose`      | `-v`      |                                | Enable verbose diagnostic logging to stderr.                | `false`   |
| `--version`      |           |                                | Show version information and exit.                          | `false`   |

## Technical Implementation Details

`hfget` operates in distinct phases to ensure efficiency and correctness.

#### 1. Fetching Phase
First, the application makes API calls to Hugging Face to get a complete, **flat list** of all files in the target repository. This provides a full manifest of the remote state.

#### 2. Analysis & Planning Phase
With the full remote file list, `hfget` builds a "download plan":
1.  **Progress Display:** An "Analyzing (xx.x%)" progress bar appears, showing the overall progress of the local file check. The percentage is accurately calculated based on the total size of all files to be verified.
2.  **Local File Check:** For each file in the remote manifest, it checks the local disk to see if a corresponding file already exists.
3.  **Verification:** If a file exists, it's verified against its expected size and (for LFS files) its SHA256 checksum. The progress bar updates as this happens.
4.  **Plan Creation:** Based on the verification results, a plan is created detailing which files to skip and which to download.

#### 3. Execution Phase
Once the plan is built and (if in interactive mode) confirmed by the user, the application executes it:
1.  **Concurrent Downloads:** For large files, the download is split into multiple chunks that are fetched simultaneously. An idle timeout ensures the download doesn't freeze on a stalled connection.
2.  **File Assembly:** Once all chunks for a file are downloaded, they are assembled into a single file on disk.
3.  **Continue on Failure:** If a file fails to download or pass verification, the error is logged, and the application continues to the next file, ensuring one bad file doesn't stop the entire job. A summary of any failures is presented at the end.

## License

This project is licensed under the MIT License.


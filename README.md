# hfget - A Robust Hugging Face Downloader

`hfget` is a lightweight, dependency-free, command-line utility for downloading models and datasets from Hugging Face. It is designed to be fast, reliable, and script-friendly, with features like concurrent downloads, file integrity verification, and interactive prompts.

## Features

  * **Concurrent Downloads:** Utilizes multiple connections to download large files in parallel, significantly speeding up the process.
  * **Integrity Verification:** Automatically verifies downloaded files against their expected size and SHA256 checksum to ensure they are not corrupted.
  * **Intelligent Syncing:** Only downloads files that are missing or have failed verification, saving time and bandwidth.
  * **Interactive & Scriptable:** Provides an interactive summary and confirmation prompt for manual use, which can be easily bypassed with a `--force` flag for use in scripts.
  * **No External Dependencies:** The compiled binary is self-contained and does not require any external libraries or runtimes.
  * **Advanced Filtering:** Download only specific files from a repository using a simple filter syntax.

## Installation

As long as you have a working Go environment, you can install `hfget` with a single command:

```sh
go install github.com/drgo/hfget/cmd/hfget@latest
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

This will download the `Llama-2-7B-GGUF` model from the user `TheBloke` into a directory named `TheBloke_Llama-2-7B-GGUF`.

```sh
hfget TheBloke/Llama-2-7B-GGUF
```

**2. Download a Dataset**

To download a dataset, you must use the `-d` flag.

```sh
hfget -d squad
```

**3. Download with Filtering**

To download only specific files (e.g., only the `q4_K_M` and `q5_K_M` GGUF files), use a colon `:` followed by comma-separated keywords.

```sh
hfget TheBloke/Llama-2-7B-GGUF:q4_K_M,q5_K_M
```

**4. Force a Re-download**

To re-download all files from a repository, regardless of their local state, use the `-f` flag. This will also skip all interactive prompts.

```sh
hfget -f TheBloke/Llama-2-7B-GGUF
```

### Command-Line Flags

| Flag               | Shorthand | Description                                                          | Default      |
| ------------------ | --------- | -------------------------------------------------------------------- | ------------ |
| `--dataset`        | `-d`      | Specify that the repository is a dataset.                            | `false`      |
| `--branch`         | `-b`      | The repository branch to download from.                              | `"main"`     |
| `--storage`        | `-s`      | The local directory where files will be saved.                       | `"./"`       |
| `--concurrent`     | `-c`      | Number of concurrent connections for downloading large files.        | `5`          |
| `--token`          | `-t`      | Your Hugging Face auth token. Can also be set via `HF_TOKEN` env var.  | `""`         |
| `--skip-sha`       | `-k`      | Skip the SHA256 checksum verification for downloaded files.          | `false`      |
| `--max-retries`    |           | Maximum number of retries for downloads on transient errors.         | `3`          |
| `--retry-interval` |           | The time to wait between retries.                                    | `5s`         |
| `--quiet`          | `-q`      | Suppress the interactive progress display and confirmation prompts.  | `false`      |
| `--force`          | `-f`      | Force re-download of all files and implies `--quiet`.                | `false`      |

## Technical Implementation Details

### How It Works: The Download Process

`hfget` operates in two distinct phases to ensure efficiency and correctness.

#### 1\. The Planning Phase

Before any files are downloaded, the application first builds a "download plan":

1.  **Fetch Metadata:** It makes an API call to Hugging Face to get the repository's metadata and a list of all files and folders at the root.
2.  **Recursive Scan:** For each subdirectory, it makes further API calls to get a complete, recursive list of all files in the repository.
3.  **Local File Check:** For each file in the remote repository, it checks the local disk to see if a corresponding file already exists.

This leads to the download decision logic.

#### 2\. The Execution Phase

Once the plan is built and confirmed by the user, the application executes it:

1.  **Concurrent Downloads:** For large files, the download is split into multiple chunks that are fetched simultaneously, maximizing bandwidth usage.
2.  **File Assembly:** Once all chunks for a file are downloaded, they are assembled into a single file on disk.
3.  **Verification:** After a file is assembled, it is immediately verified.

### Download Decision Logic

A file is only downloaded if one of the following conditions is met:

  * The file does not exist locally.
  * The local file exists, but its size does not match the size reported by the API.
  * The local file exists and its size matches, but its SHA256 checksum does not match the checksum from the API (this check only applies to LFS files and is skipped if `-k` is used).
  * The `--force` flag is used, which bypasses all of the above checks and marks every file for download.

If all files are found to be present and valid, `hfget` will inform you and ask if you wish to force a re-download anyway, giving you full control over your local repositories.

## License

This project is licensed under the MIT License.

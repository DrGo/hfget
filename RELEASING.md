# Release Process for `hfget`

This document outlines the procedure for creating and publishing a new release of the `hfget` command-line tool. The process is automated using GitHub Actions and GoReleaser.

## Prerequisites

Before starting the release process, ensure the following conditions are met:

1.  All code changes for the release have been merged into the `main` branch.
2.  The `main` branch is stable and all tests are passing.
3.  You have commit and push access to the main repository.
4.  You have decided on the new version number, following [Semantic Versioning](https://semver.org/) (e.g., v1.2.3).

## Step-by-Step Release Guide

The release process is triggered by pushing a new `git` tag to the repository. The tag must follow the pattern `vX.Y.Z`.

### Step 1: Update the Version in `main.go`

While the release build injects the version number from the git tag, it is good practice to keep the version variable in the source code up-to-date for development builds.

1.  Open `cmd/hfget/main.go`.

2.  Update the `VERSION` variable to the new version number you are about to release.

    ```go
    // File: cmd/hfget/main.go

    // Update this to the new version, e.g., "v1.1.0"
    var VERSION = "v1.1.0" 
    ```

### Step 2: Commit the Version Bump

Commit the change to the repository with a standardized message.

```sh
# Stage the change
git add cmd/hfget/main.go

# Commit with a standard version bump message
git commit -m "chore: bump version to v1.1.0"

# Push the commit to the main branch
git push origin main
```

### Step 3: Create and Push the Git Tag

This is the most critical step, as it is the trigger for the entire automated release workflow.

1.  Create a new annotated git tag. The tag **must** be prefixed with a `v`.

    ```sh
    # Replace v1.1.0 with your new version number
    git tag -a v1.1.0 -m "Release v1.1.0"
    ```

2.  Push the new tag to the GitHub repository.

    ```sh
    # Push the specific tag to the remote repository
    git push origin v1.1.0
    ```

### Step 4: Monitor the GitHub Action

Once the tag is pushed, the `Release hfget` workflow defined in `.github/workflows/release.yml` will automatically start.

1.  Navigate to the **"Actions"** tab in your GitHub repository.
2.  You will see the new workflow run at the top of the list. Click on it to monitor its progress.
3.  The workflow will execute the steps defined, including checking out the code, setting up Go, and running GoReleaser.

### Step 5: Verify the Release

When the workflow completes successfully (it should take a few minutes):

1.  Navigate to the main page of your repository and click on the **"Releases"** link on the right-hand side.
2.  You should see a new release with the title and tag of the version you just pushed.
3.  Under the **"Assets"** section, verify that all the expected files are present:
      * Compressed archives for all target platforms (`linux`, `windows`, `darwin`, `freebsd`, `openbsd`) and architectures (`amd64`, `arm64`).
      * A `checksums.txt` file containing the SHA256 hashes of all generated artifacts.
      * Source code archives (`.zip` and `.tar.gz`).

## Troubleshooting

If the GitHub Actions workflow fails:

  * Click on the failed run in the "Actions" tab to view the logs.
  * Identify the step that failed and analyze the error message. Common issues include typos in the `.goreleaser.yml` file or build errors in the code.
  * If a release was created partially, it's best to go to the "Releases" page, delete the failed release, and also delete the tag from the repository (both locally and remotely).
  * Fix the underlying issue, commit the changes, and then re-run the release process starting from Step 3.

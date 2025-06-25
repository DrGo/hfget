# Release Process for `hfget`

This document outlines the simplified procedure for creating and publishing a new release of the `hfget` command-line tool. The process is now primarily controlled by a `Makefile` and a root `VERSION` file, which then triggers the automation in GitHub Actions.

## Prerequisites

Before starting the release process, ensure the following conditions are met:

1.  All code changes for the release have been merged into the `main` branch.
2.  The `main` branch is stable and all tests are passing.
3.  You have commit and push access to the main repository.

## Step-by-Step Release Guide

The entire release process is now handled by a single `make` command.

### Step 1: Update the VERSION file

This file is the single source of truth for the release version.

1.  Open the `VERSION` file at the root of the repository.

2.  Update the content to the new version number you are about to release, following [Semantic Versioning](https://semver.org/). The version **must** be prefixed with a `v`.

    ```
    # File: VERSION
    v1.2.0
    ```

### Step 2: Create and Push the Git Tag with Make

This is the only command you need to run. It will read the `VERSION` file, commit the change, create the corresponding git tag, and push everything to the remote repository.

```sh
make tag
```

The Makefile will display its progress as it executes these steps automatically.

### Step 3: Monitor the GitHub Action

Pushing the new tag triggers the `Release hfget` workflow defined in `.github/workflows/release.yml`.

1.  Navigate to the **"Actions"** tab in your GitHub repository.
2.  You will see the new workflow run at the top of the list. Click on it to monitor its progress.
3.  The workflow will execute the steps defined, including checking out the code, setting up Go, and running GoReleaser.

### Step 4: Verify the Release

When the workflow completes successfully (it should take a few minutes):

1.  Navigate to the main page of your repository and click on the **"Releases"** link on the right-hand side.
2.  You should see a new release with the title and tag matching the version from your `VERSION` file.
3.  Under the **"Assets"** section, verify that all the expected release artifacts are present.

## Troubleshooting

If the GitHub Actions workflow fails:

  * Click on the failed run in the "Actions" tab to view the logs.
  * Identify the step that failed and analyze the error message. Common issues include typos in the `.goreleaser.yml` file or build errors in the code.
  * If a release was created partially, it's best to go to the "Releases" page, delete the failed release, and also delete the tag from the repository (both locally with `git tag -d v1.2.0` and remotely with `git push origin --delete v1.2.0`).
  * Fix the underlying issue, commit the changes, and then re-run the release process starting from Step 1.


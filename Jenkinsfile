// Jenkins pipeline for stockwatch — a port of .github/workflows/ci.yml.
//
// Deliberately Docker-free so it runs on a plain Jenkins with nothing but Java
// and a Go install on the node. It uses the system Go already on the agent's
// PATH — no container agents, no daemon, and no Jenkins-managed tool to
// configure. The image-build stage at the end is optional and skips itself when
// no Docker daemon is reachable, so the pipeline is green on a machine without
// Docker and simply does more where Docker exists.
//
// Prerequisite: `go` must be on the Jenkins node's PATH (e.g. Homebrew or an
// official install). To let Jenkins manage the toolchain instead, install the
// "Go" plugin, add a Go installation under Manage Jenkins > Tools, and add a
// `tools { go '<name>' }` block referencing it.

pipeline {
    // Runs on the built-in node (or any agent). No Docker required to allocate.
    agent any

    options {
        timestamps()
        timeout(time: 20, unit: 'MINUTES')
        // The Jenkins equivalent of Actions' concurrency cancel-in-progress.
        disableConcurrentBuilds(abortPrevious: true)
        buildDiscarder(logRotator(numToKeepStr: '20'))
    }

    environment {
        // go.mod pins the language version; refuse to silently fetch another
        // toolchain, so a mismatch fails loudly instead of drifting.
        GOTOOLCHAIN = 'local'
        // Keep the module and build caches inside the workspace. Nothing global
        // needs to be writable, and no state leaks between jobs on a shared node.
        GOPATH  = "${WORKSPACE}/.go"
        GOCACHE = "${WORKSPACE}/.gocache"
        // Prepend GOPATH/bin so `go install`ed tools (staticcheck) are found.
        PATH    = "${WORKSPACE}/.go/bin:${PATH}"
        IMAGE   = 'stockwatch:ci'
    }

    stages {
        stage('go.mod tidy') {
            // Catches a dependency added without `go mod tidy`. First, because an
            // untidy go.mod can pull in modules the committed go.sum does not
            // cover and break every later step.
            steps {
                sh '''
                    go mod tidy
                    git diff --exit-code go.mod go.sum \
                      || { echo "go.mod/go.sum are not tidy; run 'go mod tidy'"; exit 1; }
                '''
            }
        }

        stage('gofmt') {
            // Scope to this module's own sources. `gofmt .` recurses everything,
            // including the workspace-local module cache (GOPATH=.go), which holds
            // hundreds of unformatted dependency files. The go tool's `./...`
            // skips dot-dirs for us; gofmt does not, so exclude them by hand.
            steps {
                sh '''
                    unformatted=$(gofmt -l $(find . -type f -name '*.go' -not -path './.go/*' -not -path './vendor/*'))
                    if [ -n "$unformatted" ]; then
                        echo "these files are not gofmt'd:"
                        echo "$unformatted"
                        exit 1
                    fi
                '''
            }
        }

        stage('go vet') {
            steps { sh 'go vet ./...' }
        }

        stage('staticcheck') {
            // Installed into the workspace GOPATH, then run. Pinned to @latest to
            // match the Actions job.
            steps {
                sh '''
                    go install honnef.co/go/tools/cmd/staticcheck@latest
                    staticcheck ./...
                '''
            }
        }

        stage('test -race') {
            // -short skips the Postgres integration tests, which need a Docker
            // daemon (testcontainers) this pipeline intentionally does without.
            // GitHub Actions runs the full suite on a Docker-capable runner; here
            // we get the unit and adapter tests. To run integration too, give the
            // node a Postgres, set TEST_DATABASE_URL, and drop -short.
            //
            // -race needs a C compiler on the node (clang on macOS, gcc on Linux).
            steps { sh 'go test -short -race ./...' }
        }

        stage('domain coverage >= 80%') {
            // The domain package is the part worth protecting. The gate guards
            // against silently deleting tests, not against padding a number.
            steps {
                sh '''
                    go test -short -coverprofile=domain.out ./internal/inventory/
                    pct=$(go tool cover -func=domain.out | awk '/^total:/ {print substr($3, 1, length($3)-1)}')
                    echo "internal/inventory coverage: ${pct}%"
                    awk -v p="$pct" 'BEGIN { exit (p >= 80 ? 0 : 1) }' \
                      || { echo "domain coverage ${pct}% is below the 80% threshold"; exit 1; }
                '''
            }
        }

        stage('build binaries') {
            // Compile-only check; output discarded. Proves the commands build
            // without producing an artifact — that is the Docker stage's job.
            steps {
                sh '''
                    CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/server
                    CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/migrate
                '''
            }
        }

        stage('Docker image (optional)') {
            // Runs only where a Docker daemon is reachable. On a Docker-free node
            // the guard is false and the stage is skipped, not failed — so the
            // pipeline is green without Docker and builds the image where it can.
            when {
                expression { return sh(returnStatus: true, script: 'docker info >/dev/null 2>&1') == 0 }
            }
            stages {
                stage('build') {
                    steps {
                        sh '''
                            docker build \
                              --build-arg VERSION="${GIT_COMMIT:-dev}" \
                              --build-arg COMMIT="${GIT_COMMIT:-unknown}" \
                              -t "$IMAGE" .
                        '''
                    }
                }
                stage('smoke test (-version)') {
                    // A static-linking mistake builds a perfect image that exits 1
                    // on start with "no such file or directory". Only a run finds it.
                    steps { sh 'docker run --rm "$IMAGE" -version' }
                }
                stage('distroless: no shell') {
                    // The runtime base is distroless so an attacker with RCE finds
                    // no shell. If a future edit swaps in alpine, this fails.
                    steps {
                        sh '''
                            if docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'echo shell present' 2>/dev/null; then
                                echo "the runtime image contains a shell; expected distroless"
                                exit 1
                            fi
                            echo "no shell in image, as expected"
                        '''
                    }
                }
            }
            post {
                always { sh 'docker image rm "$IMAGE" 2>/dev/null || true' }
            }
        }
    }

    post {
        success { echo 'stockwatch pipeline passed: vet, staticcheck, race tests, and the domain coverage gate.' }
        failure { echo 'stockwatch pipeline failed — see the first red stage above.' }
    }
}

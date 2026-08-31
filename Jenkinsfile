// Jenkins pipeline for stockwatch — a port of .github/workflows/ci.yml.
//
// The two systems express the same intent differently, and the differences are
// the point of keeping both:
//
//   - GitHub Actions picks a runner image and adds toolchains with `uses:`
//     steps. Jenkins has no equivalent; the idiomatic way to pin a toolchain is
//     to run the stage *inside a container* whose image already carries it. So
//     where the Actions file says `setup-go`, here the stage declares
//     `agent { docker { image 'golang:1.25' } }` and Go is simply present.
//
//   - Actions groups everything in one YAML job with flat steps. Declarative
//     Jenkins nests stages, so each check below is its own stage and shows as a
//     separate box in Blue Ocean — a failing `staticcheck` is visible at a
//     glance without reading logs.
//
// Prerequisite: the Jenkins node must be able to run containers (Docker CLI on
// the node plus a reachable daemon). See the "Jenkins" section of the README for
// a one-command local setup. Without that, the `docker` agents below cannot
// start and the build fails immediately at agent allocation.

pipeline {
    // No global agent: every stage selects its own container. This keeps the
    // controller itself out of the build and means nothing runs on a shared
    // node where a stray tool version could drift from what CI pins.
    agent none

    options {
        timestamps()
        // A hung integration test should fail the build, not occupy an executor
        // for the default number of hours.
        timeout(time: 20, unit: 'MINUTES')
        // A newer commit makes an in-flight run obsolete; this is the Jenkins
        // equivalent of the Actions `concurrency: cancel-in-progress`.
        disableConcurrentBuilds(abortPrevious: true)
        buildDiscarder(logRotator(numToKeepStr: '20'))
    }

    environment {
        // go.mod pins the exact language version; refuse to silently download a
        // different toolchain, so the build fails loudly if the image drifts.
        GOTOOLCHAIN = 'local'
        IMAGE       = 'stockwatch:ci'
    }

    stages {
        stage('Verify (Go)') {
            agent {
                docker {
                    image 'golang:1.25'
                    // Run as root so `go install` and the module cache can write
                    // to /go and /root/.cache. The Docker Pipeline plugin would
                    // otherwise inject the Jenkins uid, which owns neither.
                    args  '-u root:root'
                }
            }
            stages {
                stage('go.mod tidy') {
                    // Catches a dependency added without `go mod tidy`. First,
                    // because an untidy go.mod can pull in modules the committed
                    // go.sum does not cover and break every later step.
                    steps {
                        sh '''
                            go mod tidy
                            git diff --exit-code go.mod go.sum \
                              || { echo "go.mod/go.sum are not tidy; run 'go mod tidy'"; exit 1; }
                        '''
                    }
                }

                stage('gofmt') {
                    steps {
                        sh '''
                            unformatted=$(gofmt -l .)
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
                    // No pinned-action equivalent here, so install it into the
                    // container. GOBIN is on PATH in the golang image already.
                    steps {
                        sh '''
                            go install honnef.co/go/tools/cmd/staticcheck@latest
                            staticcheck ./...
                        '''
                    }
                }

                stage('test -race') {
                    // -short skips the Postgres integration tests. They rely on
                    // testcontainers, which needs a Docker daemon this build
                    // container does not have — so this stage covers the unit and
                    // adapter tests, and GitHub Actions remains the gate that
                    // runs the full integration suite on a Docker-capable runner.
                    // To run them here too, give the node a Postgres and set
                    // TEST_DATABASE_URL, then drop -short.
                    //
                    // -race roughly doubles runtime but catches data races that
                    // are otherwise invisible until they corrupt production state.
                    steps { sh 'go test -short -race ./...' }
                }

                stage('domain coverage >= 80%') {
                    // The domain package is the part worth protecting. The gate
                    // guards against silently deleting tests, not against writing
                    // assertions purely to raise a number.
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
                    // A compile-only check. Output is discarded — the Docker
                    // stage builds the shipping artifact; this just proves the
                    // non-server commands still compile.
                    steps {
                        sh '''
                            CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/server
                            CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/migrate
                        '''
                    }
                }
            }
        }

        stage('Docker image') {
            agent {
                docker {
                    // A CLI-only image bind-mounted onto the host daemon: it can
                    // drive `docker build`/`docker run` without a daemon of its
                    // own (no docker-in-docker). Root, so it can reach the socket.
                    image 'docker:cli'
                    args  '-u root:root -v /var/run/docker.sock:/var/run/docker.sock'
                }
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
                    // Proves the binary in the image actually runs. A static-
                    // linking mistake builds a perfect image that then exits 1 on
                    // start with "no such file or directory" — only a run catches
                    // it.
                    steps { sh 'docker run --rm "$IMAGE" -version' }
                }

                stage('distroless: no shell') {
                    // The runtime base is distroless partly so an attacker with
                    // RCE finds no shell to pivot with. If a future edit swaps in
                    // alpine for convenience, this fails.
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
                // Don't leave CI-tagged images piling up on the node.
                always { sh 'docker image rm "$IMAGE" || true' }
            }
        }
    }

    post {
        success { echo 'stockwatch pipeline passed: vet, staticcheck, race tests, coverage, and a distroless image smoke test.' }
        failure { echo 'stockwatch pipeline failed — see the first red stage above.' }
    }
}

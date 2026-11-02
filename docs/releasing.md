# Releasing

A tag is a promise that the code behind it works. This is the checklist that
makes the promise true.

## Checklist

1. **Scope is complete.** The tag goes on a finished slice, not on work in
   progress. `CHANGELOG.md` describes exactly what ships.
2. **CI is green on the commit that will be tagged.** Not on an earlier commit,
   not on a branch. The exact SHA:
   ```sh
   git log --oneline -1
   # then check that this commit's CI run passed on Linux, macOS and Windows
   ```
3. **`make check` passes locally** (gofmt, vet, full test suite).
4. **The example configuration passes its own check:**
   ```sh
   make build
   ./cronkite check -config cronkite.conf.example
   ```
   Warnings are acceptable: they are the cross-platform roots that do not apply
   on this machine.
5. **The changelog has the version section** with the date.
6. **Tag and push:**
   ```sh
   git tag -a v0.1.0 -m "cronkite v0.1.0"
   git push origin v0.1.0
   ```

## What the tag produces

`.github/workflows/release.yml` runs the test suite again and then GoReleaser,
which builds static binaries for:

| OS | Architectures |
| --- | --- |
| linux | amd64, arm64 |
| darwin | amd64, arm64 |
| windows | amd64, arm64 |

Each combination ships as an archive (`tar.gz`, or `zip` on Windows) containing
the binary, the license, the changelog, the example configuration and the docs.
`checksums.txt` covers every archive.

The build identity is injected at link time, so a release binary reports the tag
it came from:

```sh
$ cronkite version
cronkite 0.1.0 (a1b2c3d)
```

A build from an untagged commit reports the commit instead (`9c6a122-dirty`),
which is how a development binary is told apart from a release one.

## Verifying a download

Never run a maintenance binary you have not verified, especially one that
deletes files:

```sh
curl -LO https://github.com/jhuancz/cronkite/releases/download/v0.1.0/checksums.txt
curl -LO https://github.com/jhuancz/cronkite/releases/download/v0.1.0/cronkite_0.1.0_linux_amd64.tar.gz
sha256sum -c checksums.txt --ignore-missing
```

## Installing without a release

Go resolves the module directly, which is enough for a pinned commit or a tag:

```sh
go install github.com/jhuancz/cronkite/cmd/cronkite@v0.1.0
```

## If a release is wrong

Do not retag. Tags are immutable by convention: consumers may have cached them.
Publish a patch release (`v0.1.1`) with the fix. If the tag itself is broken
(wrong commit, workflow failure), delete it explicitly and document why:

```sh
git push --delete origin v0.1.0
git tag -d v0.1.0
```

Only do that before anyone could reasonably have consumed it.

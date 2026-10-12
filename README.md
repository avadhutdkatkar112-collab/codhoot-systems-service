# codhoot-systems-service

Execution server for CodHoot's **native-compiler** languages — Ada, COBOL, D,
Fortran, Nim, Pascal and Assembly. One service for all seven, because all seven
install from the same Debian bookworm repositories onto the same gcc base that
`codhoot-runner-systems` uses.

C and C++ keep their own services (`codhoot-c-service`, `codhoot-cpp-service`),
which the backend reaches through per-language URLs, so this service is never
asked for them.

## Why this service exists

CodHoot's single-language services each hard-code one compiler and one entry
filename, so they cannot be reused for a language they were not built for. The
backend routes these languages here through the shared runtime image URL, and
Fortran through its toolchain family, because the backend resolves a language's
service in that order:

```
COMPILER_IMAGE_SYSTEMS_SERVICE_URL=https://codhoot-systems-service-<suffix>.onrender.com
```

Fortran is the one language here that sits on a different catalog image
(`codhoot-runner-c`) but has no service of its own, so it arrives through the
family URL instead:

```
COMPILER_FAMILY_NATIVE_SERVICE_URL=https://codhoot-systems-service-<suffix>.onrender.com
```

C and C++ have their own per-language URLs, which the backend consults first, so
setting the family URL does not move them.

The mappings are read at backend startup: set the variables, then restart the
backend service.

## Contract

`POST /compile`, signed with
`HMAC-SHA256(secret, timestamp + path + hex(sha256(body)))` keyed by
`CODHOOT_INTERNAL_SECRET`, headers `X-CodHoot-Timestamp` (unix seconds) and
`X-CodHoot-Signature` (hex digest). Unsigned, tampered or stale requests get
401.

```json
{ "language": "fortran", "source": "program p\n  print *, \"hi\"\nend program p\n" }
```

`GET /health` and `GET /health/live` are open — Render's health check and the
keepalive pinger must never need a signature.

| language | entry file  | compile                                   | run        |
|----------|-------------|-------------------------------------------|------------|
| ada      | `main.adb`  | `gnatmake -o main {src}`                  | `./main`   |
| assembly | `main.asm`  | `nasm -f elf64 -o main.o {src} && ld -o main main.o` | `./main` |
| cobol    | `main.cob`  | `cobc -x -free -o main {src}`             | `./main`   |
| d        | `main.d`    | `gdc {src} -o main`                       | `./main`   |
| fortran  | `main.f90`  | `gfortran {src} -o main`                  | `./main`   |
| nim      | `main.nim`  | `nim c --hints:off -o:main {src}`          | `./main`   |
| pascal   | `main.pas`  | `fpc -omain {src}`                        | `./main`   |

Those command lines are the backend registry's own `CompileCommand` and
`RunCommand` for each language, so a job behaves identically to the local
`codhoot-runner-systems` docker path. The Assembly entry assembles and links in
a single shell step because the registry's command is a `sh -c` line and
splitting it would hand the intermediate object file to a second sandboxed exec
for no gain.

## Toolchains

GNAT (Ada), FPC (Pascal), GDC 12 (D), gfortran, NASM + ld (Assembly), Nim and
GnuCOBOL, from Debian bookworm, plus stable-name symlinks (`gdc`, `gnatmake`) so
no version suffix leaks into the command lines. Every toolchain is smoke-tested
during the image build — nim and gnat both leave first-run caches under `HOME`,
and finding that out at build time is much cheaper than finding it out from a
student's first submission.

## Deployment

Render blueprint: **New → Blueprint**, point it at this repo, apply. Set
`CODHOOT_INTERNAL_SECRET` to the same value the backend uses.

The service listens on `PORT` (Render sets it to 10000; the image default is
8081). `MAX_CONCURRENT_JOBS` defaults to 3.

## Sandbox

Each job gets its own uid from a 32-entry pool, a private random 0700 working
directory, a minimal environment, rlimits applied by a trampoline re-exec, and
process-group kill on timeout. The service runs as root only to chown and
setuid; the boundary is the child. See `harden.go` / `harden_unix.go`.
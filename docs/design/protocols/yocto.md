I have the primary sources. Here is the brief.

---

# BitBake caching protocols — implementation brief

All line references are from **bitbake master (lib version 2.19.0)** and **openembedded-core master**, fetched from `git.openembedded.org` during this research. Release deltas are called out where they exist.

Primary sources (raw source, not docs):
- `https://git.openembedded.org/bitbake/plain/lib/hashserv/{__init__,server,client,sqlite,sqlalchemy}.py`
- `https://git.openembedded.org/bitbake/plain/lib/bb/asyncrpc/{connection,serv,client}.py`
- `https://git.openembedded.org/bitbake/plain/lib/bb/fetch2/{__init__,wget,git}.py`
- `https://git.openembedded.org/bitbake/plain/lib/bb/{siggen,cooker}.py`
- `https://git.openembedded.org/openembedded-core/plain/meta/classes-global/sstate.bbclass`
- `https://git.openembedded.org/openembedded-core/plain/meta/lib/oe/sstatesig.py`
- `https://git.openembedded.org/bitbake/plain/bin/{bitbake-hashserv,bitbake-hashclient}`

---

## TOPIC 1 — sstate mirror

### 1.1 There is no sstate protocol. It is a dumb static file server.

`sstate.bbclass` implements the mirror by **re-using the generic source fetcher with `PREMIRRORS` set to `SSTATE_MIRRORS`**. From `pstaging_fetch()` and `sstate_checkhashes()` (sstate.bbclass:715-760, 988-1010):

```python
localdata = bb.data.createCopy(d)
dldir = localdata.expand("${SSTATE_DIR}")
localdata.delVar('MIRRORS')            # MIRRORS is explicitly disabled
localdata.setVar('FILESPATH', dldir)
localdata.setVar('DL_DIR', dldir)      # DL_DIR is redirected to SSTATE_DIR
localdata.setVar('PREMIRRORS', mirrors)  # SSTATE_MIRRORS becomes PREMIRRORS
```

So: **`SSTATE_DIR` is treated as `DL_DIR`, and each sstate object is fetched as if it were a source file `file://<relative-path-in-sstate-cache>`.** Everything you know about `PREMIRRORS` applies verbatim. There is no index, no manifest, no directory listing, no negotiation. Confirmed empirically: `http://sstate.yoctoproject.org/all/` returns **403** (nginx `autoindex off`), and a HEAD on a nonexistent object returns 404. bitbake never lists directories.

### 1.2 SSTATE_MIRRORS syntax and the PATH rewriting rule

`SSTATE_MIRRORS` is a whitespace-separated list of **(regex, replacement) pairs**, parsed by `mirror_from_string()` (fetch2/__init__.py:583):

```python
def mirror_from_string(data):
    mirrors = (data or "").replace('\\n',' ').split()
    if len(mirrors) % 2 != 0:
        bb.warn('Invalid mirror data %s, should have paired members.' % data)
    return list(zip(*[iter(mirrors)]*2))
```

Canonical form (poky `local.conf.sample`, scarthgap→master):

```
SSTATE_MIRRORS ?= "\
file://.* https://someserver.tld/share/sstate/PATH;downloadfilename=PATH \
file://.* file:///some/local/dir/sstate/PATH"
```

**Where the magic tokens come from.** `build_mirroruris()` (fetch2/__init__.py:1023) builds a substitution table from the *original* URL:

```python
replacements["TYPE"]       = origud.type          # "file"
replacements["HOST"]       = origud.host          # "" for file:// URLs
replacements["PATH"]       = origud.path          # <-- the whole relative path
replacements["BASENAME"]   = origud.path.split("/")[-1]
replacements["MIRRORNAME"] = origud.host.replace(':','.') + origud.path.replace('/','.').replace('*','.')
```

`decodemirrorurl()` (fetch2/__init__.py:381-405) special-cases `file://`: `host = ""`, `path = <everything after file://>`. The sstate SRC_URI is `"file://" + sstatefile` where `sstatefile` is the *relative path inside the sstate cache* — e.g. `universal/ab/cd/sstate:zlib:...tar.zst`. Therefore:

> **`PATH` expands to the full relative sstate path including the shard directories and the `universal/` prefix.**

Result: `file://.* https://host/share/sstate/PATH` produces
`https://host/share/sstate/universal/ab/cd/sstate:zlib-native:...:14:<unihash>_populate_sysroot.tar.zst`

`;downloadfilename=PATH` is required on the **download** leg so the file lands at `SSTATE_DIR/universal/ab/cd/<name>` rather than `SSTATE_DIR/<basename>` (fetch2/__init__.py:1394: `self.localpath = self.parm["localpath"]` / `downloadfilename` sets `ud.localfile`). Without it, downloads land in the wrong place.

`uri_replace()` (fetch2/__init__.py:459) does an **anchored `re.sub(regexp, replacement, uri_decoded[loc], count=1)` per URL component** (type, host, path, user, pswd, params). Note the type regex is auto-anchored with `$` if not already (`if loc == 0 and regexp and not regexp.endswith("$"): regexp += "$"`) — added to stop `https` matching `file` → `files`.

The NATIVELSBSTRING remap case documented in the ref manual:
```
SSTATE_MIRRORS ?= "file://universal-4.9/(.*) https://server/sstate/universal-4.8/\1"
```
— i.e. the *path regex capture groups* work, because it's a plain `re.sub`.

Auth params: `;user=<u>;pswd=<p>` (uri_replace lines 490-492 treat user/pswd in the replacement as straight substitutions), or `~/.netrc` (recommended by docs).
Source: `https://git.yoctoproject.org/yocto-docs/plain/documentation/ref-manual/variables.rst` (SSTATE_MIRRORS entry, ~line 10076).

### 1.3 SSTATE_MIRROR_ALLOW_NETWORK

Only meaningful together with `BB_NO_NETWORK = "1"`. It deletes `BB_NO_NETWORK` **in the local data copy used for the sstate fetch only** (sstate.bbclass:736-738 and :1003-1005):

```python
if bb.utils.to_boolean(localdata.getVar('BB_NO_NETWORK')) and \
        bb.utils.to_boolean(localdata.getVar('SSTATE_MIRROR_ALLOW_NETWORK')):
    localdata.delVar('BB_NO_NETWORK')
```

So sstate may hit the network while all source fetching stays offline.

### 1.4 On-disk / URL layout — EXACT current scheme

From sstate.bbclass:7-48:

```
SSTATE_VERSION    = "14"
SSTATE_PKGARCH    = "${PACKAGE_ARCH}"
SSTATE_PKGSPEC    = "sstate:${PN}:${PACKAGE_ARCH}${TARGET_VENDOR}-${TARGET_OS}:${PV}:${PR}:${SSTATE_PKGARCH}:${SSTATE_VERSION}:"
SSTATE_SWSPEC     = "sstate:${PN}::${PV}:${PR}::${SSTATE_VERSION}:"
SSTATE_PKGNAME    = "${SSTATE_EXTRAPATH}${@generate_sstatefn(d.getVar('SSTATE_PKGSPEC'), d.getVar('BB_UNIHASH'), d.getVar('SSTATE_CURRTASK'), False, d)}"
SSTATE_PKG        = "${SSTATE_DIR}/${SSTATE_PKGNAME}"
SSTATE_PATHSPEC   = "${SSTATE_DIR}/${SSTATE_EXTRAPATHWILDCARD}*/*/${SSTATE_PKGSPEC}*_${SSTATE_PATH_CURRTASK}.tar.zst*"
```

and the sharding function (sstate.bbclass:14-39):

```python
def generate_sstatefn(spec, hash, taskname, siginfo, d):
    if taskname is None: return ""
    extension = ".tar.zst"
    limit = 254 - 8              # 8 chars reserved for ".siginfo"
    if siginfo:
        limit = 254
        extension = ".tar.zst.siginfo"
    if not hash: hash = "INVALID"
    fn = spec + hash + "_" + taskname + extension
    if len(fn) > limit:
        components = spec.split(":")
        # Fields 0,5,6 are mandatory, 1 is most useful, 2,3,4 are just for information
        avail = (limit - len(hash + "_" + taskname + extension) - len(components[0])
                 - len(components[1]) - len(components[5]) - len(components[6]) - 7) // 3
        components[2] = components[2][:avail]
        components[3] = components[3][:avail]
        components[4] = components[4][:avail]
        spec = ":".join(components)
        fn = spec + hash + "_" + taskname + extension
        if len(fn) > limit:
            bb.fatal("Unable to reduce sstate name to less than 255 chararacters")
    return hash[:2] + "/" + hash[2:4] + "/" + fn
```

**Therefore the full layout is:**

```
${SSTATE_DIR}/[<NATIVELSBSTRING>/]<h[0:2]>/<h[2:4]>/sstate:<PN>:<PACKAGE_ARCH><TARGET_VENDOR>-<TARGET_OS>:<PV>:<PR>:<SSTATE_PKGARCH>:<SSTATE_VERSION>:<HASH>_<task>.tar.zst
```

Critical details a re-implementer usually gets wrong:

1. **`<HASH>` is `BB_UNIHASH`, not the taskhash.** The sstate object name embeds the *unihash* produced by the hash-equivalence server. This is why the sstate mirror and the hashserv must be kept in sync (the docs warn about exactly this).
2. **Field 3 is not just `PACKAGE_ARCH`** — it is `PACKAGE_ARCH` + `TARGET_VENDOR` + `-` + `TARGET_OS`, e.g. `core2-64-poky-linux`. Field 6 (`SSTATE_PKGARCH`) is the separate arch field, e.g. `core2-64` or `x86_64` or `allarch`.
3. The two-char sharding uses **hex chars 0-1 and 2-3 of the unihash**: `ab/cd/`.
4. `SSTATE_EXTRAPATH` is `""` for normal recipes, and `"${NATIVELSBSTRING}/"` for `native`/`cross`/`crosssdk` classes (sstate.bbclass:146-148). With `uninative` enabled (the default in poky) `NATIVELSBSTRING` is literally **`universal`** (`uninative.bbclass:145: d.setVar("NATIVELSBSTRING", "universal")`). Hence `sstate-cache/universal/ab/cd/...`.
5. `do_populate_lic` (and fetch/unpack/patch/preconfigure) use `SSTATE_SWSPEC` instead, which has **empty arch fields**: `sstate:zlib:::1.3.1:r0::14:<hash>_populate_lic.tar.zst`, and `SSTATE_EXTRAPATH=""` (sstate.bbclass:190-192, and `getpathcomponents()` at :960-968 which reads the `BB_HASHFILENAME` magic string `"<isnative> <SSTATE_PKGSPEC> <SSTATE_SWSPEC>"`).

Concrete example:
```
sstate-cache/universal/9f/3c/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:9f3c8a...64hex..._populate_sysroot.tar.zst
sstate-cache/2b/7e/sstate:zlib:core2-64-poky-linux:1.3.1:r0:core2-64:14:2b7e19...64hex..._package_write_rpm.tar.zst
```

**Sidecar files that may exist next to each object:**
- `<name>.tar.zst.siginfo` — always attempted by `pstaging_fetch()` (failure is silently swallowed).
- `<name>.tar.zst.sig` — only when `SSTATE_VERIFY_SIG = "1"` (GPG detached sig, `SSTATE_SIG_KEY`).
- `<name>.tar.zst.done` — a *client-side* fetcher donestamp. Never requested from a mirror; it only shows up in a mirror if someone rsynced a build's `sstate-cache/`. `scripts/sstate-cache-management.py` explicitly enumerates these three suffixes.

The definitive grammar, straight from `scripts/sstate-cache-management.py`:
```python
SSTATE_PREFIX = "sstate:"
SSTATE_EXTENSION = ".tar.zst"
SSTATE_SUFFIXES = (SSTATE_EXTENSION, f"{SSTATE_EXTENSION}.siginfo", f"{SSTATE_EXTENSION}.done")
RE_SSTATE_PKGSPEC = re.compile(
    rf"""sstate:(?P<pn>[^:]*):
         (?P<package_target>[^:]*):
         (?P<pv>[^:]*):
         (?P<pr>[^:]*):
         (?P<sstate_pkgarch>[^:]*):
         (?P<sstate_version>[^_]*):
         (?P<bb_unihash>[^_]*)_
         (?P<bb_task>[^:]*)
         (?P<ext>(...))$""")
```

### 1.5 Release history: `.tgz` → `.tar.zst`, and SSTATE_VERSION

Measured directly by diffing release branches of `sstate.bbclass`:

| Yocto release | branch | SSTATE_VERSION | extension |
|---|---|---|---|
| 3.1 Dunfell | `dunfell` | 3 | `.tgz` |
| 3.2 Gatesgarth | `gatesgarth` | 3 | `.tgz` |
| 3.3 Hardknott | `hardknott` | 3 | `.tgz` |
| 3.4 Honister | `honister` | 7 | `.tgz` |
| **4.0 Kirkstone** | `kirkstone` | **10** | **`.tar.zst`** |
| 4.1 Langdale | `langdale` | 10 | `.tar.zst` |
| 4.2 Mickledore | `mickledore` | 11 | `.tar.zst` |
| 4.3 Nanbield | `nanbield` | 11 | `.tar.zst` |
| 5.0 Scarthgap | `scarthgap` | 12 | `.tar.zst` |
| 5.1 Styhead | `styhead` | 14 | `.tar.zst` |
| 5.2 Walnascar | `walnascar` | 14 | `.tar.zst` |
| 5.3 Whinlatter / master | `master` | 14 | `.tar.zst` |

**The zstd switch landed in Yocto 4.0 (kirkstone)**; honister and earlier are gzip tarballs. `SSTATE_ZSTD_CLEVEL ??= "8"`. Note that `SSTATE_VERSION` is *in the filename*, so objects from different releases coexist in one flat cache without collision — a Go server does not need to partition by release.

Also note the `.tgz` → `.tar.zst` change means older clients simply never ask for `.tar.zst` names, so serving a mixed cache is safe.

### 1.6 HTTP verbs and semantics — what your Go server actually sees

Two distinct phases, and they use **different HTTP clients**:

**Phase A — existence check (`sstate_checkhashes`, `BB_HASHCHECK_FUNCTION`).** Runs at the start of a build over the whole setscene task graph. For each candidate object it constructs `file://<relpath>` and calls `fetcher.checkstatus()`. For an `http(s)://` mirror this lands in `Wget.checkstatus()` (fetch2/wget.py:153+):

```python
r = urllib.request.Request(uri)
r.get_method = lambda: "HEAD"
r.add_header("Accept", "*/*")
r.add_header("User-Agent", "bitbake/{}".format(bb.__version__))
...
with opener.open(r, timeout=100) as response:
    pass
```

- **HEAD**, not GET. Pure Python `urllib`, not the `wget` binary.
- Handlers installed: `FixedHTTPRedirectHandler`, `HTTPMethodFallback`, `ProxyHandler`, `CacheHTTPHandler`, `HTTPSHandler`.
- **`HTTPMethodFallback` retries as GET on 405 — and also on 403** (`http_error_403 = http_error_405`, an explicit workaround for S3/GitHub). If your Go server returns 403 for a missing object, bitbake will re-issue a full GET.
- Redirects: 301/302/303/307/**308** are followed **preserving the HEAD method** (upstream Python <3.13 resets to GET; bitbake patches this). `Authorization`/`Cookie` headers are stripped on cross-origin redirect.
- Auth: HTTP Basic, from `;user=;pswd=` URL params, else from `~/.netrc` keyed by hostname. Header is `Authorization: Basic base64(user:pass)`.
- **Connection reuse:** `CacheHTTPHandler` + a `FetchConnectionCache` pool. `sstate_checkhashes` runs the checks in a `ThreadPoolExecutor(max_workers=BB_NUMBER_THREADS)` with a `Queue` of connection caches (sstate.bbclass:1060-1075). **Expect hundreds/thousands of pipelined HEADs over N keep-alive connections.** This is the single hottest path for an sstate mirror; it's what makes or breaks perceived performance. Honor `Connection: keep-alive`; the client drops a cached connection if the server sends `Connection: close`.
- Retries: one automatic retry on exception (`try_again=True`), then gives up quietly (`logger.debug2`, not an error — "to avoid spamming the logs in e.g. remote sstate searches").
- TLS verification can be disabled with `BB_CHECK_SSL_CERTS = "0"`.

**Phase B — download (`pstaging_fetch` → `Wget.download`).** This shells out to the **`wget` binary** (fetch2/wget.py:88-140):

```python
self.basecmd = shlex.split(d.getVar("FETCHCMD_wget") or "") or ['wget', '--tries=2', '--timeout=100']
...
fetchcmd.append("--continue")
fetchcmd.append("--directory-prefix=" + dldir)
fetchcmd += ['--progress=dot', '--verbose']
```

- `--continue` ⇒ **wget may send `Range:` headers and expect `206 Partial Content`** on retry of a partial file. Support ranges or you will get corrupt/failing resumes.
- Basic auth via `--user=`/`--password=` + `--auth-no-challenge` (i.e. **pre-emptive** Authorization header, no 401 round trip). `;redirectauth=0` suppresses this.
- Post-conditions bitbake enforces: file must exist and be **non-zero size**, else `FetchError`. So never return a 200 with an empty body for a missing object.
- URLs sent by `wget` retain the `;downloadfilename=...` param stripped (`uri = ud.url.split(";")[0]`).

The exact 3 URLs requested per object (sstate.bbclass:740-745):
```python
uris = ['file://{0};downloadfilename={0}'.format(sstatefetch),
        'file://{0}.siginfo;downloadfilename={0}.siginfo'.format(sstatefetch)]
if SSTATE_VERIFY_SIG:
    uris += ['file://{0}.sig;downloadfilename={0}.sig'.format(sstatefetch)]
```
each wrapped in `try/except bb.fetch2.BBFetchException: pass` — **failures are non-fatal**, the task just rebuilds.

### 1.7 Is there checksum verification of sstate objects?

**No content checksum.** sstate `SRC_URI`s carry no `sha256sum`, so `verify_checksum()` is a no-op for them (`supports_checksum()` requires `ud.method.supports_checksum` and a checksum to be declared). The only integrity mechanism is optional **GPG detached signatures**:

```
SSTATE_SIG_KEY ?= ""
SSTATE_VERIFY_SIG ?= "0"
```
With `SSTATE_VERIFY_SIG=1`, `sstate_installpkg()` requires `<pkg>.sig` and calls `signer.verify(sstatepkg + '.sig', d.getVar("SSTATE_VALID_SIGS"))`, warning and skipping acceleration on failure (sstate.bbclass:356, :378-386).

**Consequence: an sstate mirror is fully trusted by default.** It can inject arbitrary content into your sysroot. If your Go server is multi-tenant, this matters a lot. (Contrast with source premirrors — see §3.4.)

### 1.8 How do people *write* to a shared sstate cache?

There is **no write path in bitbake at all.** `SSTATE_DIR` is a local filesystem path; `SSTATE_MIRRORS` is read-only. Options in the wild:

- **Shared filesystem**: point `SSTATE_DIR` at an NFS/CIFS/Lustre mount. `OE_SHARED_UMASK` exists precisely for this (sstate.bbclass wraps writes in `bb.utils.umask(...)`, and objects are hardlink/rename-based so concurrent writers are mostly safe).
- **rsync after the build** (by far the most common in CI): `rsync -a --ignore-existing build/sstate-cache/ mirror:/srv/sstate/`. Yocto's own autobuilder does essentially this to populate `sstate.yoctoproject.org`.
- **`SSTATE_MIRRORS` with a writable-ish fetcher**: no — none of the fetchers upload.
- **`scripts/sstate-cache-management.py`** (was `.sh`) — a *pruning* tool that runs against a cache directory (it does not talk to a server; it globs the filesystem). Used to garbage-collect the mirror by keeping only the newest N of each `sstate:<pn>:<arch>:...:<task>` stem.
- **S3/GCS**: bitbake has `s3://` and `gs://` fetchers, so `SSTATE_MIRRORS` can read from a bucket; writes are done with `aws s3 sync` out of band.

**For your Go server:** the read side must be an ordinary HTTP static file server (HEAD + GET + Range + keep-alive + 404). The write side is entirely your invention — the pragmatic designs are (a) an HTTP `PUT`/`POST` endpoint plus a small post-build script or a `SSTATE_POST_PACKAGE` hook, or (b) exposing the same tree over WebDAV/NFS/S3-compatible API and letting the client rsync. Nothing in bitbake will authenticate a write for you; authenticated *reads* are Basic auth or netrc only.

---

## TOPIC 2 — Hash Equivalence Server (hashserv)

This is the part with an actual protocol. Everything below is from bitbake master.

### 2.1 Data model

- **taskhash** — the classic bitbake signature: hash over the task's inputs (variable values, dependency *unihashes*, file checksums). Computed by `SignatureGeneratorBasicHash`.
- **outhash** — hash over the task's *output*: the file tree it produced. Computed by `SSTATE_HASHEQUIV_METHOD`, default **`oe.sstatesig.OEOuthashBasic`** (sstate.bbclass:122). It walks the output dir deterministically and hashes metadata + content:
  ```python
  update_hash("OEOuthashBasic\n")
  if hash_version: update_hash(hash_version + "\n")     # HASHEQUIV_HASH_VERSION
  if extra_sigdata: update_hash(extra_sigdata + "\n")   # HASHEQUIV_EXTRA_SIGDATA
  update_hash("SSTATE_PKGSPEC=%s\n" % d.getVar('SSTATE_PKGSPEC'))
  update_hash("task=%s\n" % task)
  # then sorted os.walk over files: type char (d/c/b/s/l/p/-), mode, uid/gid (optionally),
  # size, and sha256 of contents...
  ```
  (sstatesig.py:539+). Note it hashes `SSTATE_PKGSPEC` and the task name into the outhash — so outputs are only "equivalent" within the same recipe/task identity.
- **unihash** — the *stable identity* used everywhere downstream, in particular in the **sstate object filename**. Initially a task's unihash == its taskhash; the server may replace it with an older, equivalent one.
  ⚠️ **`UNIHASH_REGEX = r"^[0-9a-f]{64}$"` / `is_valid_unihash()` DO NOT EXIST IN BITBAKE 2.8** (Scarthgap), which is our floor. They are a **2.10+ addition** — consistent with the version table in §2.10, and contradicting what the rest of this section used to claim. In 2.8, `handle_report` performs **no validation at all**, and upstream's own test suite reports **40-hex** unihashes (`tests.py create_test_hash` → `f46d3fbb439bd9b921095da657a4de906510d2cd`).
  ⇒ A server that enforces 64-hex **rejects a legitimate Scarthgap client.** (M3 shipped that and it failed 11 of 17 tests in the conformance gate — which is what the gate is for.) Bakery accepts `^[0-9a-f]{1,64}$`: the union of every supported release, still hex-only and length-bounded because the unihash lands in an sstate *filename*.
- **method** — a namespace string. It is the *value of `SSTATE_HASHEQUIV_METHOD`*, so on the wire it is literally **`"oe.sstatesig.OEOuthashBasic"`** (the fully-qualified Python path), **not** `"OEOuthashBasic"`. A per-task suffix may be appended via `siggen.extramethod[tid]` (rarely used). Do not assume a fixed value; treat it as an opaque string key.

**The point of the whole system:** if recipe A's task inputs change in a way that doesn't change its *output* (e.g. a whitespace-only change in a dependency), the taskhash changes but the outhash doesn't. The server maps the new taskhash to the *existing* unihash, so all downstream tasks keep their old unihashes and their old sstate objects remain valid → the rebuild stops propagating.

### 2.2 Transport & framing (CURRENT: bitbake 2.x, proto `OEHASHEQUIV 1.1`)

Three transports, all speaking the same `asyncrpc` framing:

| Address form | Transport |
|---|---|
| `unix://PATH` | AF_UNIX stream |
| `ws://HOST:PORT` / `wss://HOST:PORT` | WebSocket (text frames) |
| `HOST:PORT` or `[::1]:PORT` | raw TCP |

Parsed by `bb.asyncrpc.client.parse_address()` (asyncrpc/client.py:32). TCP sockets get `TCP_NODELAY`, `TCP_QUICKACK`, `SO_KEEPALIVE` (idle 30s, intvl 15s, cnt 4) (asyncrpc/serv.py:150-165).

**It is NOT JSON-RPC.** There is no `id`, no `jsonrpc` field, no `method`/`params` envelope. It is a line-oriented, single-key-object protocol with a stateful mode switch.

#### Handshake (client → server) — ⚠️ THE FRAMING IS TRANSPORT-DEPENDENT

**On the STREAM transports (TCP/unix), each line is `\n`-terminated:**

```
OEHASHEQUIV 1.1\n
needs-headers: false\n
[<extra-header>: <value>\n ...]
\n                              <- empty line terminates headers
```

**On WEBSOCKET, each line is ITS OWN MESSAGE, and there are NO NEWLINES AT ALL:**

```
msg 1:  OEHASHEQUIV 1.1
msg 2:  needs-headers: false
msg 3:  <EMPTY MESSAGE>          <- an empty message terminates headers
```

This is not a stylistic difference and it is very easy to miss. `setup_connection`
(asyncrpc/client.py:99-109) sends the handshake through the transport-polymorphic `send()`, and
the two transports implement it differently:

- `StreamConnection.send(msg)` → `writer.write(msg + "\n")` — newline-terminated lines.
- `WebsocketConnection.send(msg)` → `socket.send(msg)` — one WebSocket message, **no newline**.

A WebSocket server built to the stream spec — waiting for `"OEHASHEQUIV 1.1\nneeds-headers:
false\n\n"` in a single frame — **waits forever, and so does the build.** There is no error and no
timeout: bitbake simply hangs. The same asymmetry governs every subsequent message, including
stream mode's `"ok"`/`ok` pair (§2.3): over WebSocket neither carries a trailing newline.

(Recorded here because the original version of this section described only the stream framing and
presented it as universal. That is the bug that M3 nearly shipped.)

Server side (`AsyncServerConnection.process_requests`, asyncrpc/serv.py:52-80):
```python
(client_proto_name, client_proto_version) = client_protocol.split()
if client_proto_name != self.proto_name:      # "OEHASHEQUIV"
    return                                     # silently close
self.proto_version = tuple(int(v) for v in client_proto_version.split("."))
if not self.validate_proto_version(): return   # silently close
# then read headers until empty line, lowercase the tags
```
and hashserv's check (`server.py:300`):
```python
def validate_proto_version(self):
    return self.proto_version > (1, 0) and self.proto_version <= (1, 1)
```
⇒ **accept `1.1` only** (anything `>1.0` and `<=1.1`). Reject by closing the connection with no response.

If the client sent `needs-headers: true`, the server replies with its own `k: v\n` lines then a bare `\n`. hashserv's `handle_headers()` returns `{}`, so it sends just the terminating empty line. (This mechanism is used by other asyncrpc services, e.g. the PR server.)

#### Message framing — stream transports (TCP/unix)

`StreamConnection` (asyncrpc/connection.py:39-100):

```python
DEFAULT_MAX_CHUNK = 32 * 1024

def chunkify(msg, max_chunk):
    if len(msg) < max_chunk - 1:
        yield "".join((msg, "\n"))
    else:
        yield "".join((json.dumps({"chunk-stream": None}), "\n"))
        args = [iter(msg)] * (max_chunk - 1)
        for m in map("".join, itertools.zip_longest(*args, fillvalue="")):
            yield "".join(itertools.chain(m, "\n"))
        yield "\n"
```

So:
- **Small message (< 32767 chars):** one line of JSON + `\n`.
- **Large message:** first a line `{"chunk-stream": null}`, then the JSON payload split into 32767-char chunks, **each followed by `\n`**, then a **bare empty line** to terminate. The receiver concatenates the lines (stripping the `\n`s) and parses the result as JSON.
- Receiver: `recv()` does `readline()`, errors if the line doesn't end in `\n` ("Bad message"), strips the trailing newline. Empty read ⇒ `ConnectionClosedError`.
- `datetime` values are serialized with `json_serialize` → **ISO-8601 strings**.

⚠️ **The chunk payload is split by *characters*, not bytes**, and each chunk is written with `.encode("utf-8")`. This is fine for ASCII (all hashes are), but note `outhash_siginfo` can be large and non-ASCII — chunk boundaries are still character boundaries, so UTF-8 sequences aren't split. Your Go implementation should split on runes, not bytes, if it wants to be byte-identical (in practice, only the *receiver* logic matters for interop; you can send any chunk sizes as long as you emit the `chunk-stream` sentinel and terminate with an empty line — the reader just concatenates).

#### Message framing — WebSocket

`WebsocketConnection` (connection.py:103-146): **one JSON document per WebSocket text message. NO chunking, no `chunk-stream` sentinel, no trailing newline.** `send_message` = `socket.send(json.dumps(msg))`. `send()` (used in stream mode) = the raw string as one WS message. `ping_interval=None` on both server and client — bitbake disables WS pings.

This is an important asymmetry: **your Go server must implement two different framings.** The real Yocto hashserv is fronted by nginx at `https://hashserv.yoctoproject.org/ws` (verified: HTTP 400 without an `Upgrade` header, `Server: nginx/1.22.1`).

#### Request/response shape

Requests are **single-key JSON objects**: `{"<method-name>": <payload>}`. Dispatch (`server.py:317`):
```python
async def dispatch_message(self, msg):
    for k in self.handlers.keys():
        if k in msg:
            ...
            return await self.handlers[k](msg[k])
    raise bb.asyncrpc.ClientError("Unrecognized command %r" % msg)
```
It iterates the *handler dict* and takes the first key present in the message — extra keys are ignored, and there is **no request ID**; responses are strictly in-order, one per request.

Responses are bare JSON values (object, `null`, or string).

**Errors:** `{"invoke-error": {"message": "<text>"}}` — and then **the server breaks the loop and closes the connection** (serv.py:88-95). The client raises `InvokeError`. Note that `AsyncClient._send_wrapper` retries up to 3 times on `OSError/ConnectionError/ConnectionClosedError/JSONDecodeError/UnicodeDecodeError`, reconnecting each time (client.py:161-178) — but *not* on `InvokeError`.

An unrecognized command raises `ClientError`, which is **not** sent to the client; it's logged server-side and the connection is dropped.

### 2.3 Stream mode (the performance-critical part)

Three streaming commands: `get-stream`, `exists-stream`, `gc-mark-stream`. Entering stream mode:

1. Client sends `{"get-stream": null}` as a normal message.
2. Server replies via `send_message("ok")` → on the wire, **`"ok"\n`** (JSON-encoded string, *with quotes*).
3. Now the connection is in raw-line mode. Client sends bare lines; server replies with bare lines (`socket.send()` → `"%s\n" % msg`).
4. Client sends `END\n`; server replies **`ok\n`** (raw `send()`, *without quotes*) and returns to normal mode.

```python
async def _stream_handler(self, handler):
    await self.socket.send_message("ok")        # -> "ok"\n   (JSON!)
    while True:
        l = await self.socket.recv()
        if not l: break
        ...
        if l == "END": break
        msg = await handler(l)
        await self.socket.send(msg)              # -> raw line
    await self.socket.send("ok")                 # -> ok\n     (raw!)
    return self.NO_RESPONSE
```

**The `"ok"` (quoted) on entry vs `ok` (unquoted) on exit is a real, easy-to-miss wire detail.** Client side confirms it: `normal_to_stream()` uses `invoke()` (recv_message → JSON parse) and compares to `"ok"`; `stream_to_normal()` uses `socket.recv()` (raw) and compares to `"ok"` (the Python string, i.e. unquoted bytes on the wire).

Per-command line formats:

| Stream | Request line | Response line |
|---|---|---|
| `get-stream` | `<method> <taskhash>` (space-separated) | `<unihash>` or **empty string** (miss) |
| `exists-stream` | `<unihash>` | `true` / `false` |
| `gc-mark-stream` | JSON: `{"mark": "<m>", "where": {...}}` | JSON: `{"count": N}` |

**Why get-stream matters:** the client (`hashserv/client.py`, `Batch` class) fires *all* queries as fast as it can while concurrently reading responses, so the entire setscene query set costs ~1 RTT plus bandwidth instead of N RTTs:

```python
async def send_stream_batch(self, mode, msgs):
    """... sends the query messages as fast as possible, and simultaneously
    attempts to read the messages back. This helps to mitigate the effects of
    latency to the hash equivalence server by allowing multiple queries to be
    "in-flight" at once"""
```
`Batch.process()` gathers `recv()` and `send()` coroutines and asserts `len(results) == sent_count`. On reconnect, in-flight messages are **resent** to keep the result count in sync — so a Go server must never drop a request without a response, and must preserve strict ordering.

A full `bitbake core-image-minimal` issues tens of thousands of `get-stream` lookups. At 100ms RTT, non-batched that's ~1 hour; batched it's seconds. **Do not implement `get` in a loop.**

Also note `handle_get_stream` and friends are dispatched *without* the stats sampling wrapper (`if "stream" in k`) precisely because the inner loop is latency-sensitive.

### 2.4 Complete RPC method table

Registered in `ServerClient.__init__` (server.py:239-275). Read-only servers expose only the first group.

**Always available:**

| Method | Perm | Request payload | Response |
|---|---|---|---|
| `ping` | – | `{}` | `{"alive": true}` |
| `get` | `@read` | `{"taskhash": "<hex>", "method": "<str>", "all": false}` | `{"taskhash":..,"method":..,"unihash":..}` or `null`. With `"all": true`, returns the **joined outhash row**: all columns of `outhashes_v2` + `unihash` (i.e. `id, method, taskhash, outhash, created, owner, PN, PV, PR, task, outhash_siginfo, unihash`) |
| `get-outhash` | `@read` | `{"method":..,"outhash":..,"taskhash":..,"with_unihash": true}` | joined row (as above) or `null`. `with_unihash:false` → raw `outhashes_v2` row only. **See the trap below.** |
| `get-stream` | `@read` | `null` | `"ok"` then stream mode |
| `exists-stream` | `@read` | `null` | `"ok"` then stream mode |
| `get-stats` | `@read` | `null` | `{"requests": {"num":N,"total_time":f,"max_time":f,"average":f,"stdev":f}}` |
| `get-db-usage` | `@db-admin` | `{}` | `{"usage": {"unihashes_v3": {"rows": N}, "outhashes_v2": {"rows": N}, "users": {...}, "config": {...}}}` |
| `get-db-query-columns` | `@db-admin` | `{}` | `{"columns": [...]}` — all TEXT columns of both tables: `method, taskhash, unihash, gc_mark, outhash, owner, PN, PV, PR, task, outhash_siginfo` |
| `report` | `@read` (+ `@report` to actually write) | see §2.5 | `{"taskhash":..,"method":..,"unihash":..}` |
| `auth` | – | `{"username":..,"token":..}` | `{"result": true, "username":.., "permissions":[...]}` |
| `get-user` | `@user-admin` *or self* | `{"username": ".."}` (omit ⇒ self) | `{"username":..,"permissions":[..]}` or `null` |
| `get-all-users` | `@user-admin` | `{}` | `{"users": [{"username":..,"permissions":[..]}, ...]}` |
| `become-user` | `@user-admin` | `{"username": ".."}` | `{"username":..,"permissions":[..]}` |

**Only when NOT `--read-only`:**

| Method | Perm | Request | Response |
|---|---|---|---|
| `report-equiv` | `@read`+`@report` | `{"taskhash":..,"method":..,"unihash":..}` (+extra) | `{"taskhash":..,"method":..,"unihash":..}` |
| `reset-stats` | `@db-admin` | `null` | previous `{"requests": {...}}` then resets |
| `backfill-wait` | `@read` | `null` | `{"tasks": <queue size at entry>}` — **blocks until the upstream backfill queue drains** |
| `remove` | `@db-admin` | `{"where": {<col>: <val>, ...}}` | `{"count": N}` — deletes from **both** tables |
| `gc-mark` | `@db-admin` | `{"mark": "<str>", "where": {...}}` | `{"count": N}` |
| `gc-mark-stream` | `@db-admin` | `null` → stream of JSONL | `{"count": N}` per line |
| `gc-sweep` | `@db-admin` | `{"mark": "<str>"}` | `{"count": N}` |
| `gc-status` | `@db-admin` | `{}` | `{"keep": N, "remove": N, "mark": "<str>"|null}` |
| `clean-unused` | `@db-admin` | `{"max_age_seconds": N}` | `{"count": N}` |
| `refresh-token` | `@user-admin` *or self* | `{"username": ".."}` (optional) | `{"username":.., "token": "<64-char b64>"}` |
| `set-user-perms` | `@user-admin` | `{"username":.., "permissions":[..]}` | `{"username":.., "permissions":[..]}` |
| `new-user` | `@user-admin` | `{"username":.., "permissions":[..]}` | `{"username":.., "permissions":[..], "token": "<64-char>"}` |
| `delete-user` | `@user-admin` *or self* | `{"username": ".."}` | `{"username": ".."}` |

Concrete JSON examples (stream framing, TCP):

```
C: OEHASHEQUIV 1.1\n
C: needs-headers: false\n
C: \n
C: {"get": {"taskhash": "8a1c...", "method": "oe.sstatesig.OEOuthashBasic", "all": false}}\n
S: {"taskhash": "8a1c...", "method": "oe.sstatesig.OEOuthashBasic", "unihash": "31f2..."}\n

C: {"report": {"taskhash":"8a1c...","method":"oe.sstatesig.OEOuthashBasic","outhash":"c0ffee...","unihash":"8a1c...","owner":"ci@example.com","PN":"zlib","PV":"1.3.1","PR":"r0","task":"populate_sysroot","outhash_siginfo":"<...large blob...>"}}\n
S: {"taskhash": "8a1c...", "method": "oe.sstatesig.OEOuthashBasic", "unihash": "31f2..."}\n

C: {"get-stream": null}\n
S: "ok"\n
C: oe.sstatesig.OEOuthashBasic 8a1c...\n
S: 31f2...\n
C: oe.sstatesig.OEOuthashBasic deadbeef...\n
S: \n                                  <- empty line = miss
C: END\n
S: ok\n
```

⚠️ **`get-outhash` accepts a `taskhash` and then IGNORES it — do not put it in your WHERE clause.**
Both DB queries behind it key on `(method, outhash)` **alone**, ordered oldest-first
(`sqlite.py get_unihash_by_outhash` / `get_outhash`; only `get_equivalent_for_outhash` uses the
taskhash, and only to *exclude* it). The question being asked is *"has **any** task ever produced
this output?"* — which is the only form of the question that can discover an equivalence the caller
does not already know about.

Filtering by taskhash as well looks tighter and is silently catastrophic: the lookup can then only
ever return the caller's own row, so `report_readonly` — the anonymous / read-scoped path — finds an
equivalent **exactly never**. An open mirror would serve every request, return valid-looking
answers, and deliver **zero equivalence**, forever, with nothing in any log. (M3 shipped this bug and
caught it in test; hence this note.)

Also: `with_unihash` **defaults to `true`** when the key is absent
(`request.get("with_unihash", True)`), not false.

### 2.5 Equivalence logic on `report` — the core algorithm

`handle_report` (server.py:478-536). Reproduce this exactly:

```python
@permissions(READ_PERM)
async def handle_report(self, data):
    # NB: this line is 2.10+ ONLY. bitbake 2.8 (Scarthgap, our floor) does NOT validate --
    # see the unihash bullet in 2.1. Do not enforce 64-hex against a 2.8 client.
    validate_unihash(data.get("unihash"))

    if self.server.read_only or not self.user_has_permissions(REPORT_PERM):
        return await self.report_readonly(data)    # lookup only, no writes

    outhash_data = {
        "method":   data["method"],
        "outhash":  data["outhash"],
        "taskhash": data["taskhash"],
        "created":  datetime.now(),
    }
    for k in ("owner", "PN", "PV", "PR", "task", "outhash_siginfo"):
        if k in data: outhash_data[k] = data[k]
    if self.user:
        outhash_data["owner"] = self.user.username   # authenticated owner wins

    # INSERT OR IGNORE on outhashes_v2, UNIQUE(method, taskhash, outhash)
    if await self.db.insert_outhash(outhash_data):   # True iff a NEW row was created
        # Look for a *different* taskhash with the SAME outhash
        row = await self.db.get_equivalent_for_outhash(
            data["method"], data["outhash"], data["taskhash"])
        if row is not None:
            unihash = row["unihash"]                 # <-- EQUIVALENCE
        else:
            unihash = data["unihash"]                # first sighting
            if self.upstream_client is not None:     # ask upstream before minting
                upstream_data = await self.upstream_client.get_outhash(
                    data["method"], data["outhash"], data["taskhash"])
                if upstream_data is not None:
                    unihash = upstream_data["unihash"]
        # INSERT OR IGNORE on unihashes_v3, UNIQUE(method, taskhash)
        await self.insert_unihash(data["method"], data["taskhash"], unihash)

    # Always re-read: another writer may have won the race
    unihash_data = await self.get_unihash(data["method"], data["taskhash"])
    unihash = unihash_data["unihash"] if unihash_data is not None else data["unihash"]
    return {"taskhash": data["taskhash"], "method": data["method"], "unihash": unihash}
```

And the equivalence query (sqlite.py):
```sql
SELECT outhashes_v2.taskhash AS taskhash, unihashes_v3.unihash AS unihash
FROM outhashes_v2
INNER JOIN unihashes_v3
  ON unihashes_v3.method=outhashes_v2.method AND unihashes_v3.taskhash=outhashes_v2.taskhash
WHERE outhashes_v2.method=:method
  AND outhashes_v2.outhash=:outhash
  AND outhashes_v2.taskhash!=:taskhash     -- any match except the one we just inserted
ORDER BY outhashes_v2.created ASC          -- pick the OLDEST
LIMIT 1
```

**Invariants to preserve:**
1. `(method, taskhash) → unihash` is **write-once**. `insert_unihash` is `INSERT OR IGNORE`; once a taskhash has a unihash, it never changes. (Postgres backend uses `ON CONFLICT DO NOTHING`.)
2. Equivalence is only computed **when the outhash row is new**. A duplicate `report` is a cheap no-op lookup.
3. The chosen unihash is the one attached to the **oldest** `outhashes_v2` row with the same `(method, outhash)` — this is what makes unihashes stable over time and is why `created` must be recorded and monotonic-ish.
4. Read-only / unprivileged report path (`report_readonly`) does a `get_outhash(method, outhash, taskhash)` and returns that unihash if known, else **echoes back the client's own unihash**. It never writes. This is the mode the docs recommend for a hashserv paired with a fixed sstate mirror.

`report-equiv` (`handle_equivreport`) is different: it *directly asserts* `(method, taskhash) → unihash` with no outhash involved (still `INSERT OR IGNORE`), then reads back and returns whatever is actually stored. Used by `SignatureGeneratorUniHashMixIn.report_unihash_equiv` when bitbake wants to retro-map a taskhash onto a unihash it already knows (siggen.py:840).

### 2.6 Database schema

**sqlite backend** (`lib/hashserv/sqlite.py`), tables created as:

```sql
CREATE TABLE IF NOT EXISTS unihashes_v3 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    method TEXT NOT NULL, taskhash TEXT NOT NULL,
    unihash TEXT NOT NULL, gc_mark TEXT NOT NULL,
    UNIQUE(method, taskhash)
);
CREATE TABLE IF NOT EXISTS outhashes_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    method TEXT NOT NULL, taskhash TEXT NOT NULL, outhash TEXT NOT NULL,
    created DATETIME,
    owner TEXT, PN TEXT, PV TEXT, PR TEXT, task TEXT, outhash_siginfo TEXT,
    UNIQUE(method, taskhash, outhash)
);
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL, token TEXT NOT NULL, permissions TEXT NOT NULL,
    UNIQUE(username)
);
CREATE TABLE IF NOT EXISTS config (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL, value TEXT,
    UNIQUE(name)
);

CREATE INDEX IF NOT EXISTS taskhash_lookup_v4 ON unihashes_v3 (method, taskhash);
CREATE INDEX IF NOT EXISTS unihash_lookup_v1  ON unihashes_v3 (unihash);
CREATE INDEX IF NOT EXISTS outhash_lookup_v3  ON outhashes_v2 (method, outhash);
CREATE INDEX IF NOT EXISTS config_lookup      ON config (name);

PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;   -- or OFF when sync=False (bitbake's auto-spawned local server uses OFF)
```
Plus migration: drops legacy indexes `taskhash_lookup{,_v2,_v3}`, `outhash_lookup{,_v2}`, drops `tasks_v2`, and upgrades `unihashes_v2 → unihashes_v3` by copying rows with `gc_mark=''`.

**`config` holds one key that matters: `gc-mark`** (the current GC generation).

**SQLAlchemy backend** (`lib/hashserv/sqlalchemy.py`) — selected when the `-d`/`--database` value **contains `://`** (`hashserv/__init__.py:56`). Same tables/indexes declared via declarative_base, and it explicitly imports:
```python
from sqlalchemy.dialects.postgresql import insert as postgres_insert
```
⇒ **PostgreSQL is officially supported** (via `postgresql+asyncpg://user:pass@host/db`, with `--db-username`/`--db-password`), using `ON CONFLICT DO NOTHING` for the two `INSERT OR IGNORE` semantics. MySQL/others work to the extent SQLAlchemy's async dialects do, but only Postgres has a dedicated code path.

### 2.7 Garbage collection

Two-phase mark/sweep, entirely inside the DB:

- `gc-mark(mark, where)` → writes `config['gc-mark'] = mark`, then `UPDATE unihashes_v3 SET gc_mark = <current mark> WHERE <where>`. Returns rows marked. **New rows inserted after this automatically get the current mark** (`insert_unihash` writes `COALESCE((SELECT value FROM config WHERE name='gc-mark'), '')`), so a long-running mark phase doesn't lose concurrent writes.
- `gc-mark-stream` → same but one `{"mark":..,"where":{..}}` JSONL line per row; exists purely to defeat RTT (the OE autobuilder marks hundreds of thousands of unihashes).
- `gc-sweep(mark)` → refuses unless `mark == config['gc-mark']`, then `DELETE FROM unihashes_v3 WHERE gc_mark != (SELECT value FROM config WHERE name='gc-mark')` and clears the mark. (Deliberately no `COALESCE` here: if the mark is NULL, nothing is deleted.)
- `gc-status` → `{keep, remove, mark}` counts.
- `clean-unused(max_age_seconds)` → deletes orphaned outhash rows:
  ```sql
  DELETE FROM outhashes_v2 WHERE created < :oldest AND NOT EXISTS (
      SELECT unihashes_v3.id FROM unihashes_v3
      WHERE unihashes_v3.method=outhashes_v2.method
        AND unihashes_v3.taskhash=outhashes_v2.taskhash LIMIT 1)
  ```

⚠️ **Sign quirk in `clean-unused`** (server.py:657-660, current master):
```python
max_age = request["max_age_seconds"]
oldest = datetime.now() - timedelta(seconds=-max_age)   # == now + max_age
```
The double negation means a **positive** `max_age_seconds` puts the cutoff *in the future* (deleting all unused rows), while `bitbake-hashclient clean-unused N` passes N positive and its help text says "older than SECONDS old". The unit tests only ever call `clean_unused(0)`. If you want bit-compatible behavior, implement `cutoff = now + max_age_seconds`. If you want *sane* behavior, implement `now - max_age_seconds` — and be aware you'll then differ from the reference server.

### 2.8 Upstream chaining

Configured with `--upstream ADDR` (server) / `BB_HASHSERVE_UPSTREAM` (bitbake). Rules:

- `upstream and read_only` ⇒ **`ServerError`** at construction. A read-only server cannot proxy.
- **Each client connection gets its own upstream connection** (`ServerClient.process_requests` opens `create_async_client(self.server.upstream)` and closes it in `finally`). For a Go server, this means N downstream clients ⇒ N upstream connections; consider pooling, but note the reference impl does not.
- **`get` / `get-outhash` miss ⇒ synchronous upstream query + write-through:**
  ```python
  d = await self.upstream_client.get_taskhash(method, taskhash)
  await self.insert_unihash(d["method"], d["taskhash"], d["unihash"])
  ```
  (and `update_unified()` also inserts the outhash row when `all=true`/`get-outhash`).
- **`get-stream` miss ⇒ query upstream, and if hit: enqueue `(method, taskhash)` on `server.backfill_queue` and return the upstream unihash immediately.** A single background `backfill_worker_task()` coroutine drains the queue with its own upstream client and does the DB insert (validating `is_valid_unihash` first). This keeps the hot streaming path off the write path.
- `backfill-wait` → `await self.server.backfill_queue.join()`, returns the queue size seen at entry. Used to make CI deterministic.
- **`report` with upstream:** when minting a brand-new unihash, the server first asks upstream `get-outhash` and adopts upstream's unihash if it has one — so a local server can't diverge from its upstream for outputs upstream already knows.
- **`exists-stream` miss ⇒ ask upstream** (no backfill).

`BB_HASHSERVE` semantics (cooker.py:328-371):
- `"auto"` ⇒ bitbake **spawns a local hashserv as a child process** bound to `unix://${TOPDIR}/hashserve.sock`, sqlite at `${BB_HASHSERVE_DB_DIR or PERSISTENT_DIR or CACHE}/hashserv.db`, `sync=False`, `upstream=BB_HASHSERVE_UPSTREAM`. It then rewrites `BB_HASHSERVE` to that unix address for all multiconfigs. It refuses to run if the DB dir is on NFS (sqlite locking).
- Any other value ⇒ used directly as the address (`unix://`, `host:port`, `ws://`, `wss://`).
- On startup, if `BB_HASHSERVE_UPSTREAM` is set, cooker does a `client.ping()`; a `ConnectionError` ⇒ warning + upstream dropped; an `ImportError` (missing `websockets`) ⇒ **fatal**.
- Requires `BB_SIGNATURE_HANDLER = "OEEquivHash"` (bitbake.conf default) and `SSTATE_HASHEQUIV_METHOD`.

Typical topology: developer's bitbake auto-server (unix socket, sqlite, local) → `wss://hashserv.yoctoproject.org/ws` (upstream, read-only-ish, paired with `sstate.yoctoproject.org`).

### 2.9 Authentication, users, permissions, TLS

Permissions (server.py:22-36):
```
@none @read @report @db-admin @user-admin @all
DEFAULT_ANON_PERMS = (@read, @report, @db-admin)      # back-compat default!
```
`--anon-perms` restricts them. `@all` and `@user-admin` are **rejected** for anon users (`Server.__init__` raises `ServerError`). The `bitbake-hashserv` epilog says outright: *"the default Anonymous permissions are designed to not break existing server instances when upgrading, but are not particularly secure defaults."*

Token scheme:
```python
TOKEN_ALGORITHM = "sha256"; TOKEN_SIZE = 48; SALT_SIZE = 8

async def new_token():
    async with token_refresh_semaphore:      # global lock + 100ms sleep => ~10 tokens/s max
        await asyncio.sleep(0.1)
        raw = os.getrandom(TOKEN_SIZE, os.GRND_NONBLOCK)
    return base64.b64encode(raw, b"._").decode("utf-8")   # 64 chars, altchars "._", no padding

def new_salt(): return os.getrandom(SALT_SIZE, os.GRND_NONBLOCK).hex()

def hash_token(algo, salt, token):
    h = hashlib.new(algo); h.update(salt.encode()); h.update(token.encode())
    return ":".join([algo, salt, h.hexdigest()])   # stored form: "sha256:<16hex>:<64hex>"
```
`auth` compares `hash_token(algo, salt, token) == db_token` after splitting the stored value; failures `await asyncio.sleep(1)` (rate limit) then raise `InvokeError(f"Unable to authenticate as {username}")`. **Note: plain salted SHA-256, no KDF, no constant-time compare** — the token is high-entropy (288 bits) so that's defensible, but replicate the 1s delay and the global token-mint rate limit.

`become-user` = impersonation for `@user-admin`s (used by CI to attribute reports). The client re-applies it after every reconnect (`setup_connection` → `auth()` then `become_user()`), because `auth()` resets it.

Self-service: `@permissions(USER_ADMIN_PERM, allow_self_service=True, allow_anon=False)` on `refresh-token`, `get-user`, `delete-user` — a user may operate on themselves without `@user-admin`. The decorator rewrites `request["username"]` to the logged-in user when it's absent or matches.

**TLS: not implemented.** From `bin/bitbake-hashserv`:
> *"If you are using user authentication, you should run your server in websockets mode with an SSL terminating load balancer in front of it (as this server does not implement SSL). Otherwise all usernames and passwords will be transmitted in the clear."*

The **client** supports `wss://` (via the `websockets` package, min version 9.1, or 10.0 on Python ≥3.10). So: **your Go server should implement `ws://` and let a reverse proxy do TLS — or, since you're writing Go, just implement `wss://` natively; the client won't know the difference.**

Client credentials: `BB_HASHSERVE_USERNAME` / `BB_HASHSERVE_PASSWORD`, else `~/.netrc` keyed by the *server address string* (sstatesig.py:359-371).

`--reuseport` sets `SO_REUSEPORT` so you can run multiple server processes on one port for load balancing (they must share a DB — i.e. Postgres).

### 2.10 hashserv version history (what changed when)

Measured by diffing bitbake release branches (bitbake N.M ↔ Yocto release):

| bitbake | Yocto | hashserv state |
|---|---|---|
| 1.46 | 3.1 Dunfell | Synchronous `socket.makefile()` client, handshake already `OEHASHEQUIV 1.1\n\n`. `MODE_NORMAL`/`MODE_GET_STREAM` only. No asyncrpc. |
| 2.0 | 4.0 Kirkstone | Refactored onto `bb.asyncrpc`. proto `1.1`. Tables `unihashes_v2` / `outhashes_v2`. **No** websocket, **no** auth, **no** GC, **no** `exists-stream`. |
| 2.2 / 2.4 / 2.6 | 4.1 / 4.2 / 4.3 | Same as above (no auth/ws/gc). |
| **2.8** | **5.0 Scarthgap** | **Websockets, user/permission system (`auth`, `become-user`, `new-user`, `set-user-perms`, `refresh-token`), `exists-stream`, `gc-mark`/`gc-sweep`/`gc-status`, `get-db-usage`, `get-db-query-columns`, SQLAlchemy/Postgres backend, `unihashes_v3` (+`gc_mark`), split `sqlite.py`/`sqlalchemy.py`.** |
| 2.10 / 2.12 / 2.14 | 5.1 Styhead / 5.2 Walnascar / 5.3 Whinlatter | Incremental (e.g. `gc-mark-stream`, `--reuseport`, `Batch` pipelining, unihash validation regex). Protocol still `1.1`. |

**The protocol string has been `OEHASHEQUIV 1.1` since Dunfell; the *method set* is what grew.** So version-detect by probing methods, not by the handshake. There is no capability negotiation — an old client simply never sends the new methods, and a new client sending `auth` to an old server gets its connection dropped (unrecognized command ⇒ `ClientError` ⇒ close).

---

## TOPIC 3 — Source mirroring vs. package feeds

### 3.1 The variables

- **`DL_DIR`** — `${TOPDIR}/downloads` (bitbake.conf:842). Flat directory of downloaded files + `git2/` subdir for bare clones + `<file>.done` + `<file>.lock` stamps.
- **`PREMIRRORS`** — tried **before** upstream. Same `(regex, replacement)` pair syntax as `SSTATE_MIRRORS`.
- **`MIRRORS`** — tried **after** upstream fails. Default set in `meta/classes-global/mirrors.bbclass` (Debian snapshot, kernel.org, `https://downloads.yoctoproject.org/mirror/sources/`, etc.).
- Order (`Fetch.download()`, fetch2/__init__.py:1899-1960): **local `DL_DIR` (donestamp valid) → PREMIRRORS → upstream `SRC_URI` → MIRRORS.**
- **`BB_NO_NETWORK = "1"`** — `check_network_access()` raises `NetworkAccess` for any fetcher command. Note `trusted_network()` returns `True` early when `BB_NO_NETWORK` is set (it's someone else's job to block).
- **`BB_FETCH_PREMIRRORONLY = "1"`** — after PREMIRRORS are tried, sets `BB_NO_NETWORK=1` **in a copy** of the datastore, so upstream *and* MIRRORS are blocked. Fetch fails if the premirror doesn't have it. This is the "hermetic build" switch.
- **`BB_GENERATE_MIRROR_TARBALLS = "1"`** — makes the git fetcher produce `git2_<name>.tar.gz` in `DL_DIR` (`build_mirror_data`). Off by default for performance.
- **`SOURCE_MIRROR_URL` + `INHERIT += "own-mirrors"`** — `own-mirrors.bbclass` is literally just:
  ```
  PREMIRRORS:prepend = " \
  svn://.*/.*     ${SOURCE_MIRROR_URL} \
  git://.*/.*     ${SOURCE_MIRROR_URL} \
  gitsm://.*/.*   ${SOURCE_MIRROR_URL} \
  hg://.*/.*      ${SOURCE_MIRROR_URL} \
  p4://.*/.*      ${SOURCE_MIRROR_URL} \
  https?://.*/.*  ${SOURCE_MIRROR_URL} \
  ftp://.*/.*     ${SOURCE_MIRROR_URL} \
  npm://.*/?.*    ${SOURCE_MIRROR_URL} \
  s3://.*/.*      ${SOURCE_MIRROR_URL} \
  crate://.*/.*   ${SOURCE_MIRROR_URL} \
  gs://.*/.*      ${SOURCE_MIRROR_URL} \
  "
  ```
  (`https://raw.githubusercontent.com/openembedded/openembedded-core/master/meta/classes/own-mirrors.bbclass`)

### 3.2 Premirror URL layout — it's a FLAT directory

`uri_replace()` path handling (fetch2/__init__.py:499-518) is the key:

```python
if loc == 2:  # path
    basename = None
    if uri_decoded[0] != uri_replace_decoded[0] and mirrortarball:
        # source and destination url types differ => mirrortarball mapping
        basename = os.path.basename(mirrortarball)
        uri_decoded[5] = {}; uri_find_decoded[5] = {}   # kill params
    elif ud.localpath and ud.method.supports_checksum(ud):
        basename = os.path.basename(ud.localpath)
    if basename:
        uri_basename = os.path.basename(uri_decoded[loc])
        path = "/" + result_decoded[loc]
        if uri_basename and basename != uri_basename and path.endswith("/" + uri_basename):
            result_decoded[loc] = path[1:-len(uri_basename)] + basename
        elif not path.endswith("/" + basename):
            result_decoded[loc] = os.path.join(path[1:], basename)
```

⇒ The mirror URL is `<replacement>/<basename of the DL_DIR local file>`. So:

| SRC_URI | premirror request |
|---|---|
| `https://zlib.net/zlib-1.3.1.tar.gz` | `<MIRROR>/zlib-1.3.1.tar.gz` |
| `git://github.com/foo/bar.git;protocol=https;branch=main` | `<MIRROR>/git2_github.com.foo.bar.git.tar.gz` |
| `npm://registry.npmjs.org;package=lodash;version=4.17.21` | `<MIRROR>/lodash-4.17.21.tgz` |
| `https://x/y.tar.gz;downloadfilename=custom.tar.gz` | `<MIRROR>/custom.tar.gz` |

**A source mirror is a single flat directory of filenames.** No hashes, no sharding, no index. Serve it with any static file server. The same HEAD/GET/Range/405-fallback rules from §1.6 apply (it's the same `Wget` fetcher).

### 3.3 `.done` files — client-side only

`FetchData.__init__` (fetch2/__init__.py:1401-1414):
```python
if self.localpath and self.localpath.startswith(dldir):
    basepath = self.localpath
elif self.localpath:
    basepath = dldir + os.sep + os.path.basename(self.localpath)
...
self.donestamp = basepath + '.done'
```
The donestamp is a **pickled Python dict of checksums** (`pickle.Pickler(cachefile, 2)`; `verify_donestamp`/`update_stamp`, fetch2/__init__.py:685-780) — *not* an empty file (except in one legacy path in `try_mirror_url` which writes an empty `.done` "in old format" for mirror tarballs).

**Server-side: `.done` files are irrelevant. BitBake never requests a `.done` file from a mirror.** They only appear on a mirror if you rsync a `DL_DIR`/`sstate-cache` wholesale. Your Go server does not need to generate, serve, or understand them (serving them harmlessly is fine).

### 3.4 Are premirrors trusted? **No — checksums ARE verified.**

Two-layer answer:

1. Mirror `FetchData` objects are created with `newud.ignore_checksums = True` (fetch2/__init__.py:1058) — so the *mirror leg itself* doesn't verify.
2. But `Fetch.download()` immediately verifies against the **original** urldata (fetch2/__init__.py:1923-1935):
   ```python
   elif m.try_premirror(ud, self.d):
       logger.debug("Trying PREMIRRORS")
       mirrors = mirror_from_string(self.d.getVar('PREMIRRORS'))
       done = m.try_mirrors(self, ud, self.d, mirrors)
       if done:
           try:
               # early checksum verification so that if the checksum of the premirror
               # contents mismatch the fetcher can still try upstream and mirrors
               m.update_donestamp(ud, self.d)
           except ChecksumError as e:
               logger.warning("Checksum failure encountered with premirror download of %s - will attempt other sources." % u)
               done = False
   ```
   `update_stamp()` → `verify_checksum(ud, d)` → compares against `SRC_URI[sha256sum]`. On mismatch the file is renamed to `<file>_bad-checksum_<hash>` and bitbake **falls through to upstream/MIRRORS**.

⇒ **For any SRC_URI with a declared `sha256sum` (i.e. every non-SCM fetch in OE), a source premirror is cryptographically untrusted — a malicious mirror can only cause a fallback, not a compromise.** For git/SCM fetches there is no content checksum (only `SRCREV`, which git itself verifies as the commit id), so git mirror tarballs are trusted to contain the right objects but the requested SHA must exist in them.

This is the **opposite** of sstate, which has no checksums at all. Worth being explicit about in your product docs.

### 3.5 Git mirror tarballs

`Git.urldata_init` (fetch2/git.py:254-308):
```python
write_tarballs = d.getVar("BB_GENERATE_MIRROR_TARBALLS") or "0"
ud.write_tarballs = write_tarballs != "0" or ud.rebaseable
ud.write_shallow_tarballs = (d.getVar("BB_GENERATE_SHALLOW_TARBALLS") or write_tarballs) != "0"
...
gitsrcname = '%s%s' % (ud.host.replace(':', '.'),
                       ud.path.replace('/', '.').replace('*', '.').replace(' ','_')
                                .replace('(', '_').replace(')', '_'))
if gitsrcname.startswith('.'): gitsrcname = gitsrcname[1:]
if ud.rebaseable:  gitsrcname = gitsrcname + '_' + ud.revision
...
ud.clonedir  = os.path.join(gitdir, gitsrcname)         # DL_DIR/git2/<gitsrcname>
mirrortarball = 'git2_%s.tar.gz' % gitsrcname
ud.fullmirror = os.path.join(dl_dir, mirrortarball)     # DL_DIR/git2_<gitsrcname>.tar.gz
ud.mirrortarballs = [mirrortarball]
```

- `gitsrcname` = `host` (with `:`→`.`) + `path` with `/`→`.`, `*`→`.`, ` `→`_`, `(`/`)`→`_`. Leading `.` stripped.
- `git://github.com/openembedded/bitbake.git` ⇒ `git2_github.com.openembedded.bitbake.git.tar.gz`
- **The tarball contains the bare clone directory contents** (`DL_DIR/git2/<gitsrcname>/`), i.e. a full bare git repo with all refs — that's what "including the Git metadata" means. Fetching it, bitbake untars it into `git2/<gitsrcname>` and then does a normal `git fetch` against it locally.
- `rebaseable=1` in SRC_URI ⇒ one tarball per revision (`..._<sha>.tar.gz`), so force-pushes upstream don't lose the revision.
- Shallow: `BB_GIT_SHALLOW=1` (+`BB_GENERATE_SHALLOW_TARBALLS`) produces a **second, differently-named** tarball listed *first* in `ud.mirrortarballs`:
  ```
  gitshallow_<gitsrcname>[_bare][_<shallow_revs>]_<rev[:7]>[-<depth>]_<sorted refs>.tar.gz
  ```
  BitBake tries the shallow tarball before the full one.

So a source mirror serving git needs: `git2_<name>.tar.gz` and optionally `gitshallow_<...>.tar.gz`, flat, in the same directory as regular tarballs.

### 3.6 Package feeds — a completely different thing

**Source mirror** = inputs to the build (`do_fetch`, `DL_DIR`). **sstate mirror** = intermediate build artifacts (`do_*_setscene`, `SSTATE_DIR`). **Package feed** = *outputs* — the ipk/deb/rpm binaries that the **target device's package manager** installs from at runtime (or that `do_rootfs` installs from at image time).

- `PACKAGE_FEED_URIS` / `PACKAGE_FEED_BASE_PATHS` / `PACKAGE_FEED_ARCHS` are baked into the **image's** `/etc/opkg/*.conf`, `/etc/apt/sources.list`, or `/etc/yum.repos.d/*.repo` so the shipped device can `opkg update`. They are **not** used by bitbake to fetch anything during the build. The final URIs are the cross product (ref-manual variables.rst, PACKAGE_FEED_URIS entry):
  ```
  PACKAGE_FEED_URIS = "https://example.com/packagerepos/release https://example.com/packagerepos/updates"
  PACKAGE_FEED_BASE_PATHS = "rpm rpm-dev"
  PACKAGE_FEED_ARCHS = "all core2-64"
  =>
  https://example.com/packagerepos/release/rpm/all
  https://example.com/packagerepos/release/rpm/core2-64
  https://example.com/packagerepos/release/rpm-dev/all
  ... (8 total)
  ```
- `do_package_index` (recipe `meta/recipes-core/meta/package-index.bb`) calls `oe.rootfs.generate_index_files(d)`, which runs the backend indexer over `DEPLOY_DIR_{IPK,DEB,RPM}`:
  - **ipk**: `opkg-make-index` per arch dir ⇒ `${DEPLOY_DIR_IPK}/<arch>/Packages` + `Packages.gz` (+ `Packages.sig`/`.asc` when `PACKAGE_FEED_SIGN=1`, `PACKAGE_FEED_GPG_BACKEND`). Archs come from `ALL_MULTILIB_PACKAGE_ARCHS` and `SDK_PACKAGE_ARCHS`.
  - **rpm**: `createrepo_c --update -q ${DEPLOY_DIR_RPM}` ⇒ a standard `repodata/` (repomd.xml, primary/filelists/other .xml.gz), optionally GPG-signed.
  - **deb**: `apt-ftparchive`-driven, ⇒ `Packages`/`Packages.gz` per arch dir.

**A "package feed mirror" is therefore an entirely different server role from an sstate/source mirror:**

| | sstate mirror | source mirror | package feed |
|---|---|---|---|
| Consumer | bitbake (build host) | bitbake (build host) | opkg/apt/dnf (target device + `do_rootfs`) |
| Key | unihash (content-addressed-ish) | filename (+ sha256 in recipe) | package name + version + arch |
| Layout | `[universal/]ab/cd/sstate:...tar.zst` | flat dir of filenames | `<base>/<arch>/{Packages,Packages.gz}` or `repodata/` |
| Index | none (probe by HEAD) | none | **yes — mandatory, must be regenerated on every publish** |
| Verbs | HEAD, GET (+Range) | HEAD, GET (+Range) | GET (+Range); apt/dnf do conditional GETs (`If-Modified-Since`, ETag) |
| Integrity | none (optional GPG `.sig`) | sha256 from recipe | GPG-signed index (`PACKAGE_FEED_SIGN`) + per-pkg checksums *in* the index |
| Mutability | append-only, immutable objects | append-only | **mutable: the index must be atomically consistent with the pool** |

That last row is the design difference that matters: an sstate/source mirror is a pure immutable blob store where a 404 is a normal, cheap answer. A package feed is a *repository* with a metadata index that must be regenerated and published atomically, and clients cache it aggressively (so ETag/Last-Modified/Cache-Control handling is load-bearing). If you're building one Go server for all three, keep the feed path separate — it's a repository server, not a cache.

---

## Cheat-sheet for the Go implementation

**sstate endpoint** (static blob store):
- `HEAD /<prefix>/[universal/]<h0h1>/<h2h3>/<name>.tar.zst` → 200 (Content-Length) / **404** (never 403 — it triggers a wasteful GET fallback).
- `GET` same, must support `Range`/`206` (wget `--continue`), keep-alive, and never return an empty 200.
- Also serve `<name>.tar.zst.siginfo` and optionally `.sig`.
- Expect a burst of `BB_NUMBER_THREADS`-parallel HEADs on pooled connections at build start.
- Basic auth optional; no other auth mechanism exists client-side.
- Writes: your own protocol; nothing in bitbake will use it.

**hashserv endpoint**:
- Listen on TCP + unix + WebSocket. Two framings (newline/chunked vs one-JSON-per-WS-message).
- Handshake: read `OEHASHEQUIV 1.1`, read headers to blank line, reply to `needs-headers: true` with a blank line.
- Dispatch single-key JSON objects; in-order, no IDs. Errors = `{"invoke-error":{"message":..}}` + **close**.
- Implement `get-stream` with full pipelining or your users will hate you.
- Watch the `"ok"` (JSON, entering stream) vs `ok` (raw, leaving stream) asymmetry.
- Store: `unihashes_v3 UNIQUE(method,taskhash)` write-once; `outhashes_v2 UNIQUE(method,taskhash,outhash)`; equivalence = oldest matching `(method,outhash)` row with a *different* taskhash.
- Postgres is the sanctioned multi-writer backend (`--reuseport` + shared DB for horizontal scaling).

**Sources**
- [bitbake source (git.openembedded.org)](https://git.openembedded.org/bitbake/tree/lib/hashserv) · [lib/bb/asyncrpc](https://git.openembedded.org/bitbake/tree/lib/bb/asyncrpc) · [lib/bb/fetch2](https://git.openembedded.org/bitbake/tree/lib/bb/fetch2) · [bin/bitbake-hashserv](https://git.openembedded.org/bitbake/tree/bin/bitbake-hashserv)
- [openembedded-core sstate.bbclass](https://git.openembedded.org/openembedded-core/tree/meta/classes-global/sstate.bbclass) · [oe/sstatesig.py](https://git.openembedded.org/openembedded-core/tree/meta/lib/oe/sstatesig.py) · [mirrors.bbclass](https://git.openembedded.org/openembedded-core/tree/meta/classes-global/mirrors.bbclass) · [own-mirrors.bbclass](https://git.openembedded.org/openembedded-core/tree/meta/classes/own-mirrors.bbclass) · [sstate-cache-management.py](https://git.openembedded.org/openembedded-core/tree/scripts/sstate-cache-management.py)
- [Yocto: Setting up a Hash Equivalence Server](https://docs.yoctoproject.org/dev-manual/hashequivserver.html) · [Ref Manual variables glossary](https://docs.yoctoproject.org/ref-manual/variables.html) · [Efficiently Fetching Sources](https://docs.yoctoproject.org/dev-manual/efficiently-fetching-sources.html) · [BitBake user manual variables](https://docs.yoctoproject.org/bitbake/bitbake-user-manual/bitbake-user-manual-ref-variables.html)
- [poky local.conf.sample (walnascar)](https://git.yoctoproject.org/poky/tree/meta-poky/conf/templates/default/local.conf.sample?h=walnascar) · [Yocto Project Quick Build](https://docs.yoctoproject.org/brief-yoctoprojectqs/index.html)

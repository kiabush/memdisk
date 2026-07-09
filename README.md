# memDisk

A tiny RAM-backed file store for Docker.

memDisk gives you a simple HTTP API over a RAM-backed filesystem such as Docker `tmpfs` or `/dev/shm`. It is useful for temporary files, cache-like file storage, preprocessing pipelines, AI/audio/video scratch data, and short-lived artifacts.

Docker image: `kiabush/memdisk`

License: MIT

Default port: `6380`  
Default root: `/memdisk`

Data is volatile when using Docker `tmpfs`: files are lost when the container is removed or recreated.

## Features in v0.1

- RAM-backed storage using Docker `tmpfs` or `/dev/shm`
- HTTP file API
- Dynamic TTL folders such as `/tmp/30s`, `/tmp/1m`, `/tmp/10m`, `/tmp/2h`, and `/tmp/1d`
- Normal RAM files under `/files`
- Cache namespace under `/cache`
- Pinned namespace under `/pinned`
- `/stats` endpoint
- `/health` endpoint
- Upload size limit with `MEMDISK_MAX_UPLOAD`
- Dockerfile and Docker Compose example

## Quick start

```bash
docker run \
  --name memdisk \
  -p 6380:6380 \
  --tmpfs /memdisk:size=2g,mode=1777 \
  kiabush/memdisk:0.1.0
```

Or with Compose:

```bash
docker compose up --build
```

To change the RAM size, edit the `tmpfs` line in `docker-compose.yml`, for example `size=4g`.

## API examples

Upload a normal file:

```bash
curl -X PUT localhost:6380/files/hello.txt --data-binary "hello memDisk"
```

Download it:

```bash
curl localhost:6380/files/hello.txt
```

Upload a temporary file that expires after 10 minutes:

```bash
curl -X PUT localhost:6380/tmp/10m/audio.wav --data-binary @audio.wav
```

Upload a temporary file that expires after 1 minute:

```bash
curl -X PUT localhost:6380/tmp/1m/screenshot.jpg --data-binary @screenshot.jpg
```

List files:

```bash
curl localhost:6380/list
```

List under a prefix:

```bash
curl "localhost:6380/list?prefix=tmp/10m"
```

Stats:

```bash
curl localhost:6380/stats
```

Health:

```bash
curl localhost:6380/health
```

Delete:

```bash
curl -X DELETE localhost:6380/files/hello.txt
```

## Paths

```text
/memdisk
  /files       normal RAM-backed files
  /cache       cache namespace, reserved for future eviction policies
  /pinned      files that should not be auto-deleted by memDisk
  /tmp
    /{ttl}     files expire after the duration in the folder name
```

TTL is based on file modification time. The cleanup loop runs every 30 seconds by default.
The TTL folder name accepts Go-style durations such as `30s`, `1m`, `10m`, `2h`, plus day values like `1d`.

## Environment variables

| Variable | Default | Description |
|---|---:|---|
| `MEMDISK_ROOT` | `/memdisk` | Storage root path |
| `MEMDISK_PORT` | `6380` | HTTP port |
| `MEMDISK_MAX_UPLOAD` | `512m` | Max upload size per request |
| `MEMDISK_CLEANUP_INTERVAL` | `30s` | TTL cleanup interval |

## Docker Hub release

Build and tag a release:

```bash
docker build -t kiabush/memdisk:0.1.0 -t kiabush/memdisk:latest .
```

Push both tags:

```bash
docker push kiabush/memdisk:0.1.0
docker push kiabush/memdisk:latest
```

## Using `/dev/shm` instead of `tmpfs`

Recommended mode is a dedicated tmpfs mount:

```bash
docker run \
  -p 6380:6380 \
  --tmpfs /memdisk:size=2g,mode=1777 \
  kiabush/memdisk:0.1.0
```

Alternative mode using `/dev/shm`:

```bash
docker run \
  -p 6380:6380 \
  --shm-size=2g \
  -e MEMDISK_ROOT=/dev/shm/memdisk \
  kiabush/memdisk:0.1.0
```

Docker's default `/dev/shm` size is often small, so set `--shm-size` when using this mode.

## Roadmap

Likely v0.2 features:

- Snapshot/restore
- LRU eviction for `/cache`
- Prometheus metrics
- Simple token authentication

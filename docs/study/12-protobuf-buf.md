# 12 — Protobuf API design + buf

## Concepts

- **Protocol Buffers**: schema-first, binary, language-neutral serialization. The `.proto` file is your API contract between daemon and TUI.
- **Wire compatibility rules**: fields are identified by **number**, not name. Never reuse or renumber a field; `reserved` removed numbers/names. Adding fields is backward compatible.
- **buf** replaces raw `protoc`: one tool for codegen (`buf generate`), linting (`buf lint`), formatting (`buf format`), and **breaking-change detection** (`buf breaking`).

## Layout

```
proto/
└── spotd/v1/spotd.proto      # package spotd.v1 — directory must match package
gen/
└── spotd/v1/spotd.pb.go, spotd_grpc.pb.go
buf.yaml
buf.gen.yaml
```

## `buf.yaml` (v2)

```yaml
version: v2
modules:
  - path: proto
lint:
  use:
    - STANDARD
breaking:
  use:
    - FILE
```

## `buf.gen.yaml` (v2)

```yaml
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: github.com/<you>/spotd/gen
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt: paths=source_relative
  - remote: buf.build/grpc/go
    out: gen
    opt: paths=source_relative
```

- **Managed mode** sets `go_package` for you, keeping `.proto` files clean.
- Remote plugins run on the BSR (needs network, no local install). Pin versions (`buf.build/grpc/go:vX.Y.Z`) for reproducible output, or use `local: protoc-gen-go` with versions pinned via `go tool` (Go 1.24+ `tool` directive in `go.mod`).

## The spotd contract

```proto
syntax = "proto3";

package spotd.v1;

import "google/protobuf/timestamp.proto";

service PlayerService {
  rpc Play(PlayRequest) returns (PlayResponse);
  rpc Pause(PauseRequest) returns (PauseResponse);
  rpc Next(NextRequest) returns (NextResponse);
  rpc Queue(QueueRequest) returns (QueueResponse);
  // Server-streaming: live now-playing events until the client disconnects.
  rpc WatchEvents(WatchEventsRequest) returns (stream WatchEventsResponse);
}

service StatsService {
  rpc TopArtists(TopArtistsRequest) returns (TopArtistsResponse);
  rpc TopTracks(TopTracksRequest) returns (TopTracksResponse);
  rpc ListeningByDay(ListeningByDayRequest) returns (ListeningByDayResponse);
}

service PlaylistService {
  rpc ListPlaylists(ListPlaylistsRequest) returns (ListPlaylistsResponse);
  rpc StartJob(StartJobRequest) returns (StartJobResponse);
  rpc WatchJob(WatchJobRequest) returns (stream WatchJobResponse);
  rpc CancelJob(CancelJobRequest) returns (CancelJobResponse);
}

message PlayRequest {
  // Empty = resume. Otherwise a spotify:track:… URI.
  string uri = 1;
}
message PlayResponse {}

message QueueRequest { string uri = 1; }
message QueueResponse {}

message PauseRequest {}
message PauseResponse {}
message NextRequest {}
message NextResponse {}

message WatchEventsRequest {}
message WatchEventsResponse {
  oneof event {
    NowPlaying now_playing = 1;
    Stopped stopped = 2;
    HistoryUpdated history_updated = 3;
  }
}

message NowPlaying {
  string track_id = 1;
  string title = 2;
  repeated string artists = 3;
  string album = 4;
  bool is_playing = 5;
  int64 progress_ms = 6;
  int64 duration_ms = 7;
  google.protobuf.Timestamp fetched_at = 8;
}
message Stopped {}
message HistoryUpdated { int32 new_plays = 1; }

enum TimeRange {
  TIME_RANGE_UNSPECIFIED = 0;
  TIME_RANGE_LAST_7_DAYS = 1;
  TIME_RANGE_LAST_30_DAYS = 2;
  TIME_RANGE_ALL_TIME = 3;
}

message TopArtistsRequest {
  TimeRange range = 1;
  int32 limit = 2;
}
message TopArtistsResponse { repeated ArtistCount artists = 1; }
message ArtistCount {
  string id = 1;
  string name = 2;
  int64 plays = 3;
}
// … remaining messages follow the same pattern
```

### Design rules shown above (and enforced by `buf lint` STANDARD)

- Package is versioned (`spotd.v1`) and matches the directory.
- Each RPC has its **own** `XxxRequest`/`XxxResponse`, even if empty — you can add fields later without breaking.
- Enums: first value `*_UNSPECIFIED = 0`, values prefixed with the enum name.
- Services end in `Service`.
- `oneof` for event variants → type-safe switch on the client.
- Use well-known types (`Timestamp`, `Duration`) instead of raw ints when it's a time.

## Commands

```bash
buf format -w            # format
buf lint                 # style + design rules
buf generate             # codegen into gen/
buf breaking --against '.git#branch=main'   # in CI on PRs
```

## Best practices

- Treat `.proto` as public API even if only your TUI uses it — it's the CV signal.
- Document every RPC and field with comments; they flow into generated code.
- Prefer `optional` / wrapper presence only when "unset" vs "zero" truly matters.
- Don't expose DB or Spotify DTO shapes 1:1 — design messages for the client's needs.

## Pitfalls

- Reusing field numbers after deleting a field.
- Sharing one request message across RPCs → can't evolve independently.
- Committing generated code that's out of date — add `buf generate && git diff --exit-code` to CI.

## Further reading

- https://buf.build/docs/
- https://buf.build/docs/lint/rules/
- https://protobuf.dev/programming-guides/proto3/
- https://protobuf.dev/programming-guides/dos-donts/
- https://google.aip.dev/ (API design guidance)

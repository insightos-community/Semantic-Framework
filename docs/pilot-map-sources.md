# Pilot map sources (experimental)

Real Map selects a Pilot, not a Server-reachable robot service URL. The Pilot
reads its configured local service with its local credential and publishes a
snapshot to the existing authenticated `POST /pilot/v1/map-sync` endpoint.
The Server keeps the latest validated snapshot per Pilot and a separate map
subscription per project. Selection immediately projects the latest snapshot;
subsequent uploads update all subscribed projects. Source selection does not
assign a robot for motion or execute an Action.

```yaml
map_sync:
  enabled: true
  console_url: http://127.0.0.1:18050
  console_token_file: /path/to/private/console.token
  interval_seconds: 10
```

`project_id` is optional. Older Pilots may keep it: it seeds a missing project
subscription but never overrides an explicit selection or disconnect.
The public device configuration includes `service_url` without credentials,
query parameters or local token file paths. The Server does not fetch that URL.

- `GET /api/v1/projects/{id}/maps/real_map/sources` returns eligible Pilots and the
  selected binding. Only the project's owner and enrolled Pilots of that owner
  participate; managed robot project scope also applies. `available` indicates
  that a validated snapshot is stored, not that a local service health check ran.
- `PUT /api/v1/projects/{id}/maps/real_map/source` accepts
  `{"pilot_id":"pilot-instance-id"}` and returns `map_snapshot`.
- `{"pilot_id":""}` stops updates and retains the current map for reading.
  A Pilot with no snapshot is listed but cannot yet be selected. Offline Pilots
  with a stored snapshot remain selectable and are explicitly shown as offline.

Changing the coordinate source creates a new map generation and retains the
previous generation read-only. Binding and map projection commit together.
Repeated selections/uploads preserve map revisions; deletion propagates from
source snapshots. The upload acknowledgement's `revision` is the durable source
revision. A failed project projection keeps the Pilot's ETag pending for retry;
the selector exposes the project's last projection error.

The selector polls only lightweight metadata while its Real Map panel is visible.
`received_at` is the last upload, not the last local health probe: a source using
ETags does not upload unchanged maps. Source token rotation and read failures
remain diagnosed in the existing Pilot logs. Credentials never move to Web.

Tests cover ownership, missing/offline sources, old Pilot compatibility, source
switching and retention, multi-project fan-out, deletion, idempotence, concurrent
selection/upload, atomic rollback, restart persistence and invalid snapshot
rejection. Migration 35 adds source snapshots and project subscriptions without
altering existing map rows.

## Twin visuals (snapshot 1.1.0)

The map publisher may now provide `visual_assets: [{sha256, bytes, format: "glb"}]`
and an entity `properties.visual_descriptor`. The descriptor reuses the Web visual
asset contract (`visual_id`, semantic `version`, `placement_anchor`, `map_visual`).
Its `map_visual` contains only `format` and `sha256` at upload time; the Server
adds the project-scoped content URL when projecting a snapshot. The optional
`fit: "stretch"` and `heading_offset_rad` describe normalized furniture models.
Entity poses remain metric geometry-center poses in `map` coordinates.

Before posting a snapshot, Pilot checks `HEAD /pilot/v1/map-assets/{sha256}`.
Missing content is fetched from its configured map service at
`/api/semantic/map-assets/{sha256}.glb`, hash-checked, then uploaded with
`PUT /pilot/v1/map-assets/{sha256}` using the existing Pilot credential.
The Server requires an enrolled real Pilot with map sync enabled, validates
self-contained GLB 2.0 (no external resource URIs), and stores content once by
hash. Limits: 64 assets per snapshot and 2 MiB per asset. Failed uploads do not
advance the snapshot ETag. Asset ownership must match the uploading Pilot.

`GET /api/v1/projects/{id}/maps/visual-assets/{sha256}.glb` requires project
ownership and a reference in that project's current or retained map generations.
It works while the Pilot is offline, uses private immutable caching, and never
contacts a simulation Runtime. Retained generations keep their referenced assets.
Snapshot 1.0.0 and publishers without models remain supported.

`mapping.twin_scene` identifies static twin geometry (`schema_version: "1.0.0"`,
`ground_m`, `ground_source`). `mapping.display_view.rotation_deg` is exclusively
a camera/view setting. It must not rotate entity coordinates, point clouds,
selection outlines, or navigation goals a second time.

## Default real scene (schema 1.0.0)

`GET /api/v1/projects/{id}/simulation/project-scenes` also returns `real_scene`.
A project bound to a Pilot whose backend is `real` gets one default configuration:

```json
{
  "schema_version": "1.0.0",
  "scene_id": "real-scene:<project-id>",
  "project_id": "<project-id>",
  "name": "实机场景 · <robot-name>",
  "kind": "real",
  "map_id": "real_map",
  "pilot_id": "<pilot-id>",
  "robot_id": "<robot-id>",
  "robot_name": "<robot-name>"
}
```

This read model derives from the persisted Pilot map binding and registration.
It requires no scene migration, duplicate database record, or simulation runtime.
Refreshing keeps the same ID; selecting another Pilot updates its robot/source.
An offline Pilot keeps its configuration and last synchronized map available.
Unbound projects and non-real backends return `real_scene: null`; the existing
`project_scenes` simulation references are unchanged. A simulator registered via
the real Pilot interface has the same behavior, with its own robot name and ID.
Access uses the existing project ownership check.

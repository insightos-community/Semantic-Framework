// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MapSlotSimulation     = "simulation_map"
	MapSlotReal           = "real_map"
	EntityStatusActive    = "active"
	EntityStatusUncertain = "uncertain"
	EntityStatusRemoved   = "removed"
	MapSourceUser         = "user"
	MapSourceTask         = "task"
)

var ErrStaleMapGeneration = errors.New("地图 generation 已过期")

type SemanticMap struct {
	ID         string    `json:"-"`
	MapID      string    `json:"map_id"`
	ProjectID  string    `json:"project_id"`
	Slot       string    `json:"slot"`
	Generation int64     `json:"generation"`
	Revision   int64     `json:"revision"`
	FrameID    string    `json:"frame_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type MapGeneration struct {
	MapID      string     `json:"-"`
	Generation int64      `json:"generation"`
	Revision   int64      `json:"revision"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}
type Quaternion struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
	W float64 `json:"w"`
}
type Pose struct {
	Position    Position   `json:"position"`
	Orientation Quaternion `json:"orientation"`
}
type Bounds struct {
	Kind   string     `json:"kind"`
	Size   *Position  `json:"size,omitempty"`
	Radius float64    `json:"radius,omitempty"`
	Height float64    `json:"height,omitempty"`
	Points []Position `json:"points,omitempty"`
}

type MapEntity struct {
	MapID           string          `json:"-"`
	Generation      int64           `json:"generation"`
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Name            string          `json:"name"`
	Status          string          `json:"status"`
	FrameID         string          `json:"frame_id"`
	Pose            Pose            `json:"pose"`
	Bounds          Bounds          `json:"bounds"`
	Properties      map[string]any  `json:"properties"`
	Source          string          `json:"source"`
	SourceTimestamp *time.Time      `json:"source_timestamp,omitempty"`
	Evidence        []string        `json:"evidence"`
	ManualFields    map[string]bool `json:"manual_fields,omitempty"`
	Revision        int64           `json:"revision"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type MapRelation struct {
	MapID      string    `json:"-"`
	Generation int64     `json:"generation"`
	ID         string    `json:"id"`
	SubjectID  string    `json:"subject_id"`
	Predicate  string    `json:"predicate"`
	ObjectID   string    `json:"object_id"`
	Source     string    `json:"source"`
	Evidence   []string  `json:"evidence"`
	Manual     bool      `json:"manual"`
	Revision   int64     `json:"revision"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type MapSnapshot struct {
	Map         SemanticMap    `json:"map"`
	Generation  MapGeneration  `json:"generation_view"`
	ReadOnly    bool           `json:"read_only"`
	Entities    []MapEntity    `json:"entities"`
	Relations   []MapRelation  `json:"relations"`
	NameMatches map[string]int `json:"name_matches,omitempty"`
}

type MapSourceEntity struct {
	SourceID string    `json:"source_id"`
	Entity   MapEntity `json:"entity"`
}

type MapUpdate struct {
	Generation        int64             `json:"generation"`
	ExpectedRevision  int64             `json:"expected_revision"`
	Source            string            `json:"source"`
	Entities          []MapEntity       `json:"entities,omitempty"`
	RemoveEntityIDs   []string          `json:"remove_entity_ids,omitempty"`
	Relations         []MapRelation     `json:"relations,omitempty"`
	SourceEntities    []MapSourceEntity `json:"source_entities,omitempty"`
	RemoveRelationIDs []string          `json:"remove_relation_ids,omitempty"`
}

type MapQuery struct {
	Generation  int64
	EntityID    string
	EntityIDs   []string
	EntityNames []string
	EntityType  string
	EntityTypes []string
	Status      string
	RegionID    string
	Predicate   string
	SubjectID   string
	ObjectID    string
	Limit       int
}

func NewMapEntityID() string        { return "entity-" + uuid.NewString() }
func NewMapRelationID() string      { return "relation-" + uuid.NewString() }
func ValidMapSlot(slot string) bool { return slot == MapSlotSimulation || slot == MapSlotReal }
func validEntityStatus(status string) bool {
	return status == EntityStatusActive || status == EntityStatusUncertain || status == EntityStatusRemoved
}
func stableMapPredicate(predicate string) bool {
	switch strings.ToLower(strings.TrimSpace(predicate)) {
	case "near", "left_of", "right_of", "in_front_of", "behind", "visible", "reachable", "distance":
		return false
	default:
		return strings.TrimSpace(predicate) != ""
	}
}
func finite(v float64) bool            { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func validatePosition(p Position) bool { return finite(p.X) && finite(p.Y) && finite(p.Z) }
func validatePose(p Pose) error {
	if !validatePosition(p.Position) || !finite(p.Orientation.X) || !finite(p.Orientation.Y) || !finite(p.Orientation.Z) || !finite(p.Orientation.W) {
		return fmt.Errorf("位姿必须是有限数值: %w", ErrInvalidState)
	}
	norm := math.Sqrt(p.Orientation.X*p.Orientation.X + p.Orientation.Y*p.Orientation.Y + p.Orientation.Z*p.Orientation.Z + p.Orientation.W*p.Orientation.W)
	if math.Abs(norm-1) > 1e-3 {
		return fmt.Errorf("四元数必须归一化且顺序为 xyzw: %w", ErrInvalidState)
	}
	return nil
}
func validateBounds(b Bounds) error {
	switch b.Kind {
	case "", "point":
		return nil
	case "box":
		if b.Size == nil || !validatePosition(*b.Size) || b.Size.X <= 0 || b.Size.Y <= 0 || b.Size.Z < 0 {
			return fmt.Errorf("box size 非法: %w", ErrInvalidState)
		}
	case "cylinder":
		if !finite(b.Radius) || !finite(b.Height) || b.Radius <= 0 || b.Height < 0 {
			return fmt.Errorf("cylinder 尺寸非法: %w", ErrInvalidState)
		}
	case "plane", "region", "polygon":
		if len(b.Points) < 3 {
			return fmt.Errorf("区域至少需要三个点: %w", ErrInvalidState)
		}
		for _, p := range b.Points {
			if !validatePosition(p) {
				return fmt.Errorf("区域点非法: %w", ErrInvalidState)
			}
		}
	default:
		return fmt.Errorf("不支持的 bounds.kind: %w", ErrInvalidState)
	}
	return nil
}

const semanticMapColumns = `id, project_id, slot, generation, revision, frame_id, created_at, updated_at`

func scanSemanticMap(row workflowScanner) (SemanticMap, error) {
	var v SemanticMap
	err := row.Scan(&v.ID, &v.ProjectID, &v.Slot, &v.Generation, &v.Revision, &v.FrameID, &v.CreatedAt, &v.UpdatedAt)
	v.MapID = v.Slot
	return v, err
}

func ensureSemanticMapsTx(tx *sql.Tx, projectID string, now time.Time) error {
	for _, slot := range []string{MapSlotSimulation, MapSlotReal} {
		id := "map-" + uuid.NewString()
		res, err := tx.Exec(`INSERT OR IGNORE INTO semantic_maps (id,project_id,slot,generation,revision,frame_id,created_at,updated_at) VALUES (?,?,?,1,0,'world',?,?)`, id, projectID, slot, now, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if _, err := tx.Exec(`INSERT INTO map_generations (map_id,generation,reason,created_at,revision) VALUES (?,1,'initial',?,0)`, id, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// EnsureSemanticMaps 是迁移/修复入口；正常 Project 创建在同一事务内初始化地图。
func (s *Store) EnsureSemanticMaps(projectID string, now time.Time) ([]SemanticMap, error) {
	if _, err := s.GetProject(projectID); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureSemanticMapsTx(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListSemanticMaps(projectID)
}
func (s *Store) ListSemanticMaps(projectID string) ([]SemanticMap, error) {
	rows, err := s.db.Query(`SELECT `+semanticMapColumns+` FROM semantic_maps WHERE project_id=? ORDER BY slot`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SemanticMap, 0, 2)
	for rows.Next() {
		v, e := scanSemanticMap(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) GetSemanticMap(projectID, slot string) (SemanticMap, error) {
	if !ValidMapSlot(slot) {
		return SemanticMap{}, ErrInvalidState
	}
	v, err := scanSemanticMap(s.db.QueryRow(`SELECT `+semanticMapColumns+` FROM semantic_maps WHERE project_id=? AND slot=?`, projectID, slot))
	if errors.Is(err, sql.ErrNoRows) {
		return SemanticMap{}, ErrNotFound
	}
	return v, err
}
func (s *Store) GetSemanticMapByID(id string) (SemanticMap, error) {
	v, err := scanSemanticMap(s.db.QueryRow(`SELECT `+semanticMapColumns+` FROM semantic_maps WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SemanticMap{}, ErrNotFound
	}
	return v, err
}

const entityColumns = `map_id,generation,id,type,name,status,frame_id,pose_json,bounds_json,properties_json,source,source_ts,evidence_json,manual_json,revision,updated_at`

func scanMapEntity(row workflowScanner) (MapEntity, error) {
	var v MapEntity
	var pose, bounds, properties, evidence, manual string
	err := row.Scan(&v.MapID, &v.Generation, &v.ID, &v.Type, &v.Name, &v.Status, &v.FrameID, &pose, &bounds, &properties, &v.Source, &v.SourceTimestamp, &evidence, &manual, &v.Revision, &v.UpdatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(pose), &v.Pose)
	}
	if err == nil {
		err = json.Unmarshal([]byte(bounds), &v.Bounds)
	}
	if err == nil {
		err = json.Unmarshal([]byte(properties), &v.Properties)
	}
	if err == nil {
		err = json.Unmarshal([]byte(evidence), &v.Evidence)
	}
	if err == nil {
		err = json.Unmarshal([]byte(manual), &v.ManualFields)
	}
	if v.Properties == nil {
		v.Properties = map[string]any{}
	}
	if v.ManualFields == nil {
		v.ManualFields = map[string]bool{}
	}
	return v, err
}

const relationColumns = `map_id,generation,id,subject_id,predicate,object_id,source,evidence_json,manual,revision,updated_at`

func scanMapRelation(row workflowScanner) (MapRelation, error) {
	var v MapRelation
	var evidence string
	var manual int
	err := row.Scan(&v.MapID, &v.Generation, &v.ID, &v.SubjectID, &v.Predicate, &v.ObjectID, &v.Source, &evidence, &manual, &v.Revision, &v.UpdatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(evidence), &v.Evidence)
	}
	v.Manual = manual != 0
	return v, err
}

func (s *Store) getMapGeneration(mapID string, generation int64) (MapGeneration, error) {
	var value MapGeneration
	err := s.db.QueryRow(`SELECT map_id,generation,revision,reason,created_at,closed_at FROM map_generations WHERE map_id=? AND generation=?`, mapID, generation).Scan(
		&value.MapID, &value.Generation, &value.Revision, &value.Reason, &value.CreatedAt, &value.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MapGeneration{}, ErrNotFound
	}
	return value, err
}

func (s *Store) GetMapSnapshot(projectID, slot string, generation int64) (MapSnapshot, error) {
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return MapSnapshot{}, err
	}
	if generation == 0 {
		generation = m.Generation
	}
	generationView, err := s.getMapGeneration(m.ID, generation)
	if err != nil {
		return MapSnapshot{}, err
	}
	entities, err := s.queryMapEntities(m.ID, generation, MapQuery{Generation: generation})
	if err != nil {
		return MapSnapshot{}, err
	}
	relations, err := s.queryMapRelations(m.ID, generation, MapQuery{Generation: generation})
	if err != nil {
		return MapSnapshot{}, err
	}
	readOnly := generation != m.Generation || generationView.ClosedAt != nil
	m.Generation = generation
	m.Revision = generationView.Revision
	if generationView.ClosedAt != nil {
		m.UpdatedAt = *generationView.ClosedAt
	}
	return MapSnapshot{Map: m, Generation: generationView, ReadOnly: readOnly, Entities: entities, Relations: relations}, nil
}
func (s *Store) QuerySemanticMap(projectID, slot string, q MapQuery) (MapSnapshot, error) {
	if q.EntityID == "" && len(q.EntityIDs) == 0 && len(q.EntityNames) == 0 && q.EntityType == "" && len(q.EntityTypes) == 0 && q.Status == "" && q.RegionID == "" &&
		q.Predicate == "" && q.SubjectID == "" && q.ObjectID == "" {
		return MapSnapshot{}, fmt.Errorf("map.query 必须提供至少一个过滤条件: %w", ErrInvalidState)
	}
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return MapSnapshot{}, err
	}
	if q.Generation == 0 {
		q.Generation = m.Generation
	}
	generationView, err := s.getMapGeneration(m.ID, q.Generation)
	if err != nil {
		return MapSnapshot{}, err
	}
	entities, err := s.queryMapEntities(m.ID, q.Generation, q)
	if err != nil {
		return MapSnapshot{}, err
	}
	relations, err := s.queryMapRelations(m.ID, q.Generation, q)
	if err != nil {
		return MapSnapshot{}, err
	}
	if q.RegionID != "" {
		entities, err = s.filterEntitiesInRegion(m.ID, q.Generation, q.RegionID, entities)
		if err != nil {
			return MapSnapshot{}, err
		}
	}
	var nameMatches map[string]int
	if len(q.EntityNames) > 0 {
		nameMatches = make(map[string]int, len(q.EntityNames))
		for _, name := range q.EntityNames {
			nameMatches[name] = 0
		}
		for _, entity := range entities {
			nameMatches[entity.Name]++
		}
	}
	entities, relations, err = s.limitMapQuerySubset(m.ID, q.Generation, q, entities, relations)
	if err != nil {
		return MapSnapshot{}, err
	}
	readOnly := q.Generation != m.Generation || generationView.ClosedAt != nil
	m.Generation, m.Revision = q.Generation, generationView.Revision
	if generationView.ClosedAt != nil {
		m.UpdatedAt = *generationView.ClosedAt
	}
	return MapSnapshot{Map: m, Generation: generationView, ReadOnly: readOnly, Entities: entities, Relations: relations, NameMatches: nameMatches}, nil
}

// limitMapQuerySubset 防止 map.query 在命中少量 Entity/Relation 时把整张地图
// 顺带返回给模型。Entity 条件保留与命中 Entity 相连的关系，并补齐这些关系
// 的另一端；Relation 条件则只保留返回关系实际引用的 Entity。
func (s *Store) limitMapQuerySubset(mapID string, generation int64, q MapQuery,
	entities []MapEntity, relations []MapRelation) ([]MapEntity, []MapRelation, error) {
	entityFilter := q.EntityID != "" || len(q.EntityIDs) > 0 || len(q.EntityNames) > 0 || q.EntityType != "" || len(q.EntityTypes) > 0 || q.Status != "" || q.RegionID != ""
	relationFilter := q.Predicate != "" || q.SubjectID != "" || q.ObjectID != ""
	if !entityFilter && !relationFilter {
		return entities, relations, nil
	}
	// limit 先作用于 seed，再扩展关系端点。若在扩展后分别截断 Entity 和
	// Relation，会留下指向未返回 Entity 的悬空关系。
	if q.Limit > 0 && entityFilter && len(entities) > q.Limit {
		entities = entities[:q.Limit]
	}
	if q.Limit > 0 && len(relations) > q.Limit {
		relations = relations[:q.Limit]
	}
	selected := make(map[string]struct{}, len(entities))
	for _, entity := range entities {
		selected[entity.ID] = struct{}{}
	}
	if entityFilter {
		filtered := make([]MapRelation, 0, len(relations))
		for _, relation := range relations {
			_, subjectSelected := selected[relation.SubjectID]
			_, objectSelected := selected[relation.ObjectID]
			if subjectSelected || objectSelected {
				filtered = append(filtered, relation)
				selected[relation.SubjectID] = struct{}{}
				selected[relation.ObjectID] = struct{}{}
			}
		}
		relations = filtered
	}
	if relationFilter {
		selected = make(map[string]struct{}, len(relations)*2)
		for _, relation := range relations {
			selected[relation.SubjectID] = struct{}{}
			selected[relation.ObjectID] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return []MapEntity{}, relations, nil
	}
	all, err := s.queryMapEntities(mapID, generation, MapQuery{Generation: generation})
	if err != nil {
		return nil, nil, err
	}
	result := make([]MapEntity, 0, len(selected))
	for _, entity := range all {
		if _, ok := selected[entity.ID]; ok {
			result = append(result, entity)
		}
	}
	return result, relations, nil
}
func (s *Store) queryMapEntities(mapID string, generation int64, q MapQuery) ([]MapEntity, error) {
	query := `SELECT ` + entityColumns + ` FROM map_entities WHERE map_id=? AND generation=?`
	args := []any{mapID, generation}
	if q.EntityID != "" {
		query += ` AND id=?`
		args = append(args, q.EntityID)
	}
	if len(q.EntityIDs) > 0 {
		query += ` AND id IN (` + strings.TrimRight(strings.Repeat("?,", len(q.EntityIDs)), ",") + `)`
		for _, id := range q.EntityIDs {
			args = append(args, id)
		}
	}
	if q.EntityType != "" {
		// Type remains independent from an exact name filter.
		query += ` AND type=?`
		args = append(args, q.EntityType)
	}
	if len(q.EntityTypes) > 0 {
		query += ` AND type IN (` + strings.TrimRight(strings.Repeat("?,", len(q.EntityTypes)), ",") + `)`
		for _, entityType := range q.EntityTypes {
			args = append(args, entityType)
		}
	}
	if q.Status != "" {
		if !validEntityStatus(q.Status) {
			return nil, ErrInvalidState
		}
		query += ` AND status=?`
		args = append(args, q.Status)
	}
	if len(q.EntityNames) > 0 {
		query += ` AND name IN (` + strings.TrimRight(strings.Repeat("?,", len(q.EntityNames)), ",") + `)`
		for _, name := range q.EntityNames {
			args = append(args, name)
		}
	}
	query += ` ORDER BY type,name,id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MapEntity, 0)
	for rows.Next() {
		v, e := scanMapEntity(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) queryMapRelations(mapID string, generation int64, q MapQuery) ([]MapRelation, error) {
	query := `SELECT r.map_id,r.generation,r.id,r.subject_id,r.predicate,r.object_id,r.source,r.evidence_json,r.manual,r.revision,r.updated_at FROM map_relations r JOIN map_entities subject ON subject.map_id=r.map_id AND subject.generation=r.generation AND subject.id=r.subject_id AND subject.status<>'removed' JOIN map_entities object ON object.map_id=r.map_id AND object.generation=r.generation AND object.id=r.object_id AND object.status<>'removed' WHERE r.map_id=? AND r.generation=?`
	args := []any{mapID, generation}
	if q.Predicate != "" {
		query += ` AND r.predicate=?`
		args = append(args, q.Predicate)
	}
	if q.SubjectID != "" {
		query += ` AND r.subject_id=?`
		args = append(args, q.SubjectID)
	}
	if q.ObjectID != "" {
		query += ` AND r.object_id=?`
		args = append(args, q.ObjectID)
	}
	query += ` ORDER BY r.predicate,r.subject_id,r.object_id,r.id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MapRelation, 0)
	for rows.Next() {
		v, e := scanMapRelation(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) filterEntitiesInRegion(mapID string, generation int64, regionID string, items []MapEntity) ([]MapEntity, error) {
	region, err := scanMapEntity(s.db.QueryRow(`SELECT `+entityColumns+` FROM map_entities WHERE map_id=? AND generation=? AND id=? AND status<>?`, mapID, generation, regionID, EntityStatusRemoved))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if region.Bounds.Kind != "region" && region.Bounds.Kind != "plane" && region.Bounds.Kind != "box" {
		return nil, ErrInvalidState
	}
	result := make([]MapEntity, 0)
	for _, item := range items {
		if pointInsideBounds(item.Pose.Position, region.Pose.Position, region.Bounds) {
			result = append(result, item)
		}
	}
	return result, nil
}
func pointInsideBounds(p, center Position, b Bounds) bool {
	if b.Kind == "box" && b.Size != nil {
		return math.Abs(p.X-center.X) <= b.Size.X/2 && math.Abs(p.Y-center.Y) <= b.Size.Y/2 && math.Abs(p.Z-center.Z) <= math.Max(b.Size.Z/2, 1e-6)
	}
	if len(b.Points) < 3 {
		return false
	}
	inside := false
	j := len(b.Points) - 1
	for i := 0; i < len(b.Points); i++ {
		pi, pj := b.Points[i], b.Points[j]
		if (pi.Y > p.Y) != (pj.Y > p.Y) && p.X < (pj.X-pi.X)*(p.Y-pi.Y)/(pj.Y-pi.Y)+pi.X {
			inside = !inside
		}
		j = i
	}
	return inside
}

func (s *Store) CreateMapGeneration(projectID, slot string, expectedRevision int64, reason string, now time.Time) (SemanticMap, error) {
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return SemanticMap{}, err
	}
	if m.Revision != expectedRevision {
		return SemanticMap{}, ErrRevisionConflict
	}
	tx, err := s.db.Begin()
	if err != nil {
		return SemanticMap{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE map_generations SET closed_at=? WHERE map_id=? AND generation=?`, now, m.ID, m.Generation); err != nil {
		return SemanticMap{}, err
	}
	res, err := tx.Exec(`UPDATE semantic_maps SET generation=generation+1,revision=0,updated_at=? WHERE id=? AND revision=?`, now, m.ID, expectedRevision)
	if err != nil {
		return SemanticMap{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return SemanticMap{}, ErrRevisionConflict
	}
	if _, err := tx.Exec(`INSERT INTO map_generations (map_id,generation,reason,created_at,revision) VALUES (?,?,?,?,0)`, m.ID, m.Generation+1, strings.TrimSpace(reason), now); err != nil {
		return SemanticMap{}, err
	}
	if err := tx.Commit(); err != nil {
		return SemanticMap{}, err
	}
	return s.GetSemanticMap(projectID, slot)
}

func (s *Store) ApplyMapUpdate(projectID, slot string, update MapUpdate, now time.Time) (MapSnapshot, error) {
	if update.Source != "user" && update.Source != "task" && update.Source != "simulation" && update.Source != "observation" {
		return MapSnapshot{}, fmt.Errorf("地图更新来源非法: %w", ErrInvalidState)
	}
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return MapSnapshot{}, err
	}
	if update.Generation != m.Generation {
		return MapSnapshot{}, ErrStaleMapGeneration
	}
	if update.ExpectedRevision != m.Revision {
		return MapSnapshot{}, ErrRevisionConflict
	}
	tx, err := s.db.Begin()
	if err != nil {
		return MapSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, sourceEntity := range update.SourceEntities {
		if update.Source == MapSourceUser || strings.TrimSpace(sourceEntity.SourceID) == "" {
			return MapSnapshot{}, ErrInvalidState
		}
		entity := sourceEntity.Entity
		var mappedID string
		err := tx.QueryRow(`SELECT entity_id FROM map_source_mappings WHERE map_id=? AND generation=? AND source=? AND source_id=?`, m.ID, m.Generation, update.Source, sourceEntity.SourceID).Scan(&mappedID)
		if errors.Is(err, sql.ErrNoRows) {
			if entity.ID == "" {
				entity.ID = NewMapEntityID()
			}
			mappedID = entity.ID
			if _, err := tx.Exec(`INSERT INTO map_source_mappings (map_id,generation,source,source_id,entity_id) VALUES (?,?,?,?,?)`, m.ID, m.Generation, update.Source, sourceEntity.SourceID, mappedID); err != nil {
				return MapSnapshot{}, err
			}
		} else if err != nil {
			return MapSnapshot{}, err
		} else if entity.ID != "" && entity.ID != mappedID {
			return MapSnapshot{}, fmt.Errorf("来源对象不能改绑到另一个 Entity: %w", ErrInvalidState)
		}
		entity.ID = mappedID
		update.Entities = append(update.Entities, entity)
	}
	for i := range update.Entities {
		entity := update.Entities[i]
		if entity.ID == "" {
			entity.ID = NewMapEntityID()
		}
		if entity.Status == "" {
			entity.Status = EntityStatusActive
		}
		if !validEntityStatus(entity.Status) || strings.TrimSpace(entity.Type) == "" || strings.TrimSpace(entity.Name) == "" {
			return MapSnapshot{}, ErrInvalidState
		}
		if entity.FrameID == "" {
			entity.FrameID = m.FrameID
		}
		if err := validatePose(entity.Pose); err != nil {
			return MapSnapshot{}, err
		}
		if err := validateBounds(entity.Bounds); err != nil {
			return MapSnapshot{}, err
		}
		existing, getErr := getMapEntityTx(tx, m.ID, m.Generation, entity.ID)
		if getErr == nil && update.Source != MapSourceUser {
			preserveManual(&entity, existing)
		} else if getErr != nil && !errors.Is(getErr, ErrNotFound) {
			return MapSnapshot{}, getErr
		}
		if update.Source == MapSourceUser {
			entity.ManualFields = map[string]bool{"existence": true, "type": true, "name": true, "frame_id": true, "pose": true, "bounds": true, "properties": true}
		}
		if entity.Properties == nil {
			entity.Properties = map[string]any{}
		}
		pose, _ := json.Marshal(entity.Pose)
		bounds, _ := json.Marshal(entity.Bounds)
		properties, _ := json.Marshal(entity.Properties)
		evidence, _ := json.Marshal(entity.Evidence)
		manual, _ := json.Marshal(entity.ManualFields)
		_, err = tx.Exec(`INSERT INTO map_entities (map_id,generation,id,type,name,status,frame_id,pose_json,bounds_json,properties_json,source,source_ts,evidence_json,manual_json,revision,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?) ON CONFLICT(map_id,generation,id) DO UPDATE SET type=excluded.type,name=excluded.name,status=excluded.status,frame_id=excluded.frame_id,pose_json=excluded.pose_json,bounds_json=excluded.bounds_json,properties_json=excluded.properties_json,source=excluded.source,source_ts=excluded.source_ts,evidence_json=excluded.evidence_json,manual_json=excluded.manual_json,revision=map_entities.revision+1,updated_at=excluded.updated_at`, m.ID, m.Generation, entity.ID, entity.Type, entity.Name, entity.Status, entity.FrameID, string(pose), string(bounds), string(properties), update.Source, entity.SourceTimestamp, string(evidence), string(manual), now)
		if err != nil {
			return MapSnapshot{}, err
		}
	}
	for _, id := range update.RemoveEntityIDs {
		existing, err := getMapEntityTx(tx, m.ID, m.Generation, id)
		if err != nil {
			return MapSnapshot{}, err
		}
		if update.Source != MapSourceUser && existing.ManualFields["existence"] {
			return MapSnapshot{}, fmt.Errorf("来源 %s 不能移除人工 Entity %s: %w", update.Source, id, ErrInvalidState)
		}
		if _, err := tx.Exec(`UPDATE map_entities SET status=?,revision=revision+1,updated_at=? WHERE map_id=? AND generation=? AND id=?`, EntityStatusRemoved, now, m.ID, m.Generation, id); err != nil {
			return MapSnapshot{}, err
		}
	}
	for i := range update.Relations {
		relation := update.Relations[i]
		if relation.ID == "" {
			relation.ID = NewMapRelationID()
		}
		if relation.SubjectID == "" || relation.ObjectID == "" || relation.SubjectID == relation.ObjectID || !stableMapPredicate(relation.Predicate) {
			return MapSnapshot{}, ErrInvalidState
		}
		existingRelation, relationErr := getMapRelationTx(tx, m.ID, m.Generation, relation.ID)
		if relationErr == nil && existingRelation.Manual && update.Source != MapSourceUser {
			return MapSnapshot{}, fmt.Errorf("来源 %s 不能覆盖人工 Relation %s: %w", update.Source, relation.ID, ErrInvalidState)
		}
		if relationErr != nil && !errors.Is(relationErr, ErrNotFound) {
			return MapSnapshot{}, relationErr
		}
		if err := activeEntitiesExistTx(tx, m.ID, m.Generation, relation.SubjectID, relation.ObjectID); err != nil {
			return MapSnapshot{}, err
		}
		if update.Source == MapSourceUser {
			relation.Manual = true
		}
		evidence, _ := json.Marshal(relation.Evidence)
		_, err = tx.Exec(`INSERT INTO map_relations (map_id,generation,id,subject_id,predicate,object_id,source,evidence_json,manual,revision,updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?) ON CONFLICT(map_id,generation,id) DO UPDATE SET subject_id=excluded.subject_id,predicate=CASE WHEN map_relations.manual=1 AND excluded.manual=0 THEN map_relations.predicate ELSE excluded.predicate END,object_id=excluded.object_id,source=excluded.source,evidence_json=excluded.evidence_json,manual=MAX(map_relations.manual,excluded.manual),revision=map_relations.revision+1,updated_at=excluded.updated_at`, m.ID, m.Generation, relation.ID, relation.SubjectID, relation.Predicate, relation.ObjectID, update.Source, string(evidence), relation.Manual, now)
		if err != nil {
			return MapSnapshot{}, err
		}
	}
	for _, id := range update.RemoveRelationIDs {
		if _, err := tx.Exec(`DELETE FROM map_relations WHERE map_id=? AND generation=? AND id=? AND (manual=0 OR ?=?)`, m.ID, m.Generation, id, update.Source, MapSourceUser); err != nil {
			return MapSnapshot{}, err
		}
	}
	res, err := tx.Exec(`UPDATE semantic_maps SET revision=revision+1,updated_at=? WHERE id=? AND generation=? AND revision=?`, now, m.ID, m.Generation, m.Revision)
	if err != nil {
		return MapSnapshot{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return MapSnapshot{}, ErrRevisionConflict
	}
	if _, err := tx.Exec(`UPDATE map_generations SET revision=revision+1 WHERE map_id=? AND generation=?`, m.ID, m.Generation); err != nil {
		return MapSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return MapSnapshot{}, err
	}
	return s.GetMapSnapshot(projectID, slot, m.Generation)
}
func getMapRelationTx(tx *sql.Tx, mapID string, generation int64, id string) (MapRelation, error) {
	v, err := scanMapRelation(tx.QueryRow(`SELECT `+relationColumns+` FROM map_relations WHERE map_id=? AND generation=? AND id=?`, mapID, generation, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MapRelation{}, ErrNotFound
	}
	return v, err
}

func getMapEntityTx(tx *sql.Tx, mapID string, generation int64, id string) (MapEntity, error) {
	v, err := scanMapEntity(tx.QueryRow(`SELECT `+entityColumns+` FROM map_entities WHERE map_id=? AND generation=? AND id=?`, mapID, generation, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MapEntity{}, ErrNotFound
	}
	return v, err
}
func preserveManual(incoming *MapEntity, existing MapEntity) {
	if existing.ManualFields["existence"] {
		incoming.Status = existing.Status
	}
	if existing.ManualFields["type"] {
		incoming.Type = existing.Type
	}
	if existing.ManualFields["name"] {
		incoming.Name = existing.Name
	}
	if existing.ManualFields["frame_id"] {
		incoming.FrameID = existing.FrameID
	}
	if existing.ManualFields["pose"] {
		incoming.Pose = existing.Pose
	}
	if existing.ManualFields["bounds"] {
		incoming.Bounds = existing.Bounds
	}
	if existing.ManualFields["properties"] {
		incoming.Properties = existing.Properties
	}
	// Evidence 只保存 Artifact/Trace 等稳定引用。Observation 或 Task 更新可以
	// 追加新证据，但不能静默抹掉历史引用；用户更新仍可显式整理完整列表。
	incoming.Evidence = mergeMapEvidence(existing.Evidence, incoming.Evidence)
	incoming.ManualFields = existing.ManualFields
}

func mergeMapEvidence(existing, incoming []string) []string {
	if len(existing) == 0 && len(incoming) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, values := range [][]string{existing, incoming} {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}
func activeEntitiesExistTx(tx *sql.Tx, mapID string, generation int64, ids ...string) error {
	for _, id := range ids {
		var status string
		err := tx.QueryRow(`SELECT status FROM map_entities WHERE map_id=? AND generation=? AND id=?`, mapID, generation, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) || status == EntityStatusRemoved {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ResolveMapSourceEntity(projectID, slot string, generation int64, source, sourceID string) (string, error) {
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return "", err
	}
	if generation != m.Generation {
		return "", ErrStaleMapGeneration
	}
	var entityID string
	err = s.db.QueryRow(`SELECT entity_id FROM map_source_mappings WHERE map_id=? AND generation=? AND source=? AND source_id=?`, m.ID, generation, source, sourceID).Scan(&entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if err := s.ValidateMapReference(projectID, slot, generation, []string{entityID}); err != nil {
		return "", err
	}
	return entityID, nil
}

func (s *Store) ResolveMapEntitySource(
	projectID, slot string, generation int64, source, entityID string,
) (string, error) {
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return "", err
	}
	if generation != m.Generation {
		return "", ErrStaleMapGeneration
	}
	if err := s.ValidateMapReference(projectID, slot, generation, []string{entityID}); err != nil {
		return "", err
	}
	var sourceID string
	err = s.db.QueryRow(
		`SELECT source_id FROM map_source_mappings
		 WHERE map_id=? AND generation=? AND source=? AND entity_id=?`,
		m.ID, generation, source, entityID,
	).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return sourceID, err
}

func (s *Store) ValidateMapReference(projectID, slot string, generation int64, entityIDs []string) error {
	if !ValidMapSlot(slot) {
		return ErrInvalidState
	}
	m, err := s.GetSemanticMap(projectID, slot)
	if err != nil {
		return err
	}
	if m.Generation != generation {
		return ErrStaleMapGeneration
	}
	for _, id := range entityIDs {
		var status string
		err := s.db.QueryRow(`SELECT status FROM map_entities WHERE map_id=? AND generation=? AND id=?`, m.ID, generation, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) || status == EntityStatusRemoved {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return nil
}

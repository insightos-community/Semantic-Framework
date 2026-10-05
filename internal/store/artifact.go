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
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// artifactURIScheme 是产物引用 URI 的方案名（架构文档 05 §9：tool result 只返回引用）。
const artifactURIScheme = "artifact://"

// Artifact 是一条工具产物记录：元数据落库（artifacts 表），内容本体存
// 文件系统（artifactDir/<id>），URI 是两者之上的统一引用。
// 为什么内容与元数据分离：产物本体可能是图像/大文件，会话与工具结果
// 只携带引用与摘要，内容按需加载（架构文档 05 §9）。
type Artifact struct {
	// ID 产物唯一标识（art- 前缀 + uuid）。
	ID string

	// OwnerID 用户上传产物的归属用户；系统工具产物为空。
	OwnerID string

	// MediaType 内容的媒体类型（如 text/markdown、image/jpeg）。
	MediaType string

	// URI 产物引用（artifact://<id>）。
	URI string

	// Summary 产物的文本摘要（放进工具结果，供模型理解内容）。
	Summary string

	// Metadata 附加元数据 JSON（来源、位置等；无则 "{}"）。
	Metadata string

	// Size 内容字节数。
	Size int64

	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// NewArtifactID 生成产物 ID（art- 前缀 + uuid）。
func NewArtifactID() string {
	return "art-" + uuid.NewString()
}

// ArtifactDir 返回产物内容本体的存放目录（Open 时由 SQLite 路径推导）。
func (s *Store) ArtifactDir() string {
	return s.artifactDir
}

// PutArtifact 写入一个产物：ID 自动生成后委托 PutArtifactWithID。
func (s *Store) PutArtifact(mediaType, summary, metadata string, content []byte) (Artifact, error) {
	return s.putArtifactWithOwner(NewArtifactID(), "", mediaType, summary, metadata, content)
}

// PutUserArtifact 写入带用户归属的上传产物。
func (s *Store) PutUserArtifact(ownerID, mediaType, summary, metadata string, content []byte) (Artifact, error) {
	return s.putArtifactWithOwner(NewArtifactID(), ownerID, mediaType, summary, metadata, content)
}

// PutArtifactWithID 以指定 ID 写入产物：reduction 外置等场景在写入前
// 已生成引用并展示给模型（引用即产物 ID），需要按既有 ID 落库。
// 先写内容文件，再落元数据行。
// 为什么这个顺序：元数据行是对内容文件的引用，先有内容再有引用，
// 引用才不会悬空；落库失败时尽力清理已写文件，不遗留孤儿内容。
func (s *Store) PutArtifactWithID(id, mediaType, summary, metadata string, content []byte) (Artifact, error) {
	return s.putArtifactWithOwner(id, "", mediaType, summary, metadata, content)
}

func (s *Store) putArtifactWithOwner(id, ownerID, mediaType, summary, metadata string, content []byte) (Artifact, error) {
	if err := os.MkdirAll(s.artifactDir, 0o755); err != nil {
		return Artifact{}, fmt.Errorf("创建产物目录 %s 失败: %w", s.artifactDir, err)
	}
	if err := os.WriteFile(s.artifactPath(id), content, 0o644); err != nil {
		return Artifact{}, fmt.Errorf("写入产物内容 %q 失败: %w", id, err)
	}

	a := Artifact{
		ID:        id,
		OwnerID:   ownerID,
		MediaType: mediaType,
		URI:       artifactURIScheme + id,
		Summary:   summary,
		Metadata:  metadata,
		Size:      int64(len(content)),
		CreatedAt: time.Now().UTC(),
	}
	if a.Metadata == "" {
		a.Metadata = "{}"
	}
	if _, err := s.db.Exec(
		`INSERT INTO artifacts (id, media_type, uri, summary, metadata, size, created_at, owner_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.MediaType, a.URI, a.Summary, a.Metadata, a.Size, a.CreatedAt, a.OwnerID,
	); err != nil {
		if rmErr := os.Remove(s.artifactPath(id)); rmErr != nil {
			s.logger.WithError(rmErr).Warn("产物落库失败，清理内容文件失败", "artifact_id", id)
		}
		return Artifact{}, fmt.Errorf("写入产物元数据 %q 失败: %w", id, err)
	}
	return a, nil
}

// PutUserArtifactStream 将 Pilot 上传内容直接写入 Artifact 文件空间。maxBytes
// 限制单个文件，整个过程中不把图像、点云或视频读入 Server 内存。
func (s *Store) PutUserArtifactStream(ownerID, mediaType, summary, metadata string, source io.Reader, maxBytes int64) (Artifact, error) {
	if maxBytes <= 0 {
		return Artifact{}, errors.New("Artifact stream maxBytes 必须大于 0")
	}
	if err := os.MkdirAll(s.artifactDir, 0o755); err != nil {
		return Artifact{}, err
	}
	id := NewArtifactID()
	finalPath := s.artifactPath(id)
	temporaryPath := finalPath + ".partial"
	output, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("创建 Artifact 临时文件失败: %w", err)
	}
	size, copyErr := io.Copy(output, io.LimitReader(source, maxBytes+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || size > maxBytes {
		_ = os.Remove(temporaryPath)
		if size > maxBytes {
			return Artifact{}, fmt.Errorf("Artifact 超过 %d 字节", maxBytes)
		}
		if copyErr != nil {
			return Artifact{}, copyErr
		}
		return Artifact{}, closeErr
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		_ = os.Remove(temporaryPath)
		return Artifact{}, err
	}
	_ = os.Chmod(finalPath, 0o640)
	artifact := Artifact{ID: id, OwnerID: ownerID, MediaType: mediaType, URI: artifactURIScheme + id,
		Summary: summary, Metadata: metadata, Size: size, CreatedAt: time.Now().UTC()}
	if artifact.Metadata == "" {
		artifact.Metadata = "{}"
	}
	if _, err := s.db.Exec(`INSERT INTO artifacts (id, media_type, uri, summary, metadata, size, created_at, owner_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, artifact.MediaType, artifact.URI, artifact.Summary,
		artifact.Metadata, artifact.Size, artifact.CreatedAt, artifact.OwnerID); err != nil {
		_ = os.Remove(finalPath)
		return Artifact{}, fmt.Errorf("写入产物元数据 %q 失败: %w", id, err)
	}
	return artifact, nil
}

// OpenArtifactContent 为 HTTP Bridge 返回只读文件；调用方必须关闭。大文件
// 下载由 io.Copy 流式发送，不能调用 GetArtifact 把内容完整载入内存。
func (s *Store) OpenArtifactContent(id string) (Artifact, *os.File, error) {
	artifact, err := s.GetArtifactMeta(id)
	if err != nil {
		return Artifact{}, nil, err
	}
	file, err := os.Open(s.artifactPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, nil, ErrNotFound
	}
	if err != nil {
		return Artifact{}, nil, err
	}
	return artifact, file, nil
}

// GetArtifact 按 ID 读取产物元数据与内容本体；任一不存在返回 ErrNotFound。
func (s *Store) GetArtifact(id string) (Artifact, []byte, error) {
	a, err := s.GetArtifactMeta(id)
	if err != nil {
		return Artifact{}, nil, err
	}
	content, err := os.ReadFile(s.artifactPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, nil, ErrNotFound
	}
	if err != nil {
		return Artifact{}, nil, fmt.Errorf("读取产物内容 %q 失败: %w", id, err)
	}
	return a, content, nil
}

// GetArtifactMeta 按 ID 查询产物元数据（不含内容），不存在时返回 ErrNotFound。
func (s *Store) GetArtifactMeta(id string) (Artifact, error) {
	var a Artifact
	err := s.db.QueryRow(
		`SELECT id, media_type, uri, summary, metadata, size, created_at, owner_id FROM artifacts WHERE id = ?`, id,
	).Scan(&a.ID, &a.MediaType, &a.URI, &a.Summary, &a.Metadata, &a.Size, &a.CreatedAt, &a.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("查询产物 %q 失败: %w", id, err)
	}
	return a, nil
}

// ListArtifacts 按创建时间倒序查询产物元数据；limit <= 0 时返回全部。
func (s *Store) ListArtifacts(limit, offset int) ([]Artifact, error) {
	query := `SELECT id, media_type, uri, summary, metadata, size, created_at, owner_id
		FROM artifacts ORDER BY created_at DESC, id`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询产物列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var artifacts []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.MediaType, &a.URI, &a.Summary, &a.Metadata, &a.Size, &a.CreatedAt, &a.OwnerID); err != nil {
			return nil, fmt.Errorf("扫描产物记录失败: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历产物列表失败: %w", err)
	}
	return artifacts, nil
}

// ListArtifactsByOwner 按用户归属列出 Artifact 元数据，避免前端资源面看到
// 其他用户或系统内部产物。limit <= 0 时返回该用户全部记录。
func (s *Store) ListArtifactsByOwner(ownerID string, limit, offset int) ([]Artifact, error) {
	query := `SELECT id, media_type, uri, summary, metadata, size, created_at, owner_id
		FROM artifacts WHERE owner_id = ? ORDER BY created_at DESC, id`
	args := []any{ownerID}
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询用户 Artifact 列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var artifacts []Artifact
	for rows.Next() {
		var artifact Artifact
		if err := rows.Scan(&artifact.ID, &artifact.MediaType, &artifact.URI,
			&artifact.Summary, &artifact.Metadata, &artifact.Size, &artifact.CreatedAt,
			&artifact.OwnerID); err != nil {
			return nil, fmt.Errorf("扫描用户 Artifact 失败: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户 Artifact 失败: %w", err)
	}
	return artifacts, nil
}

// ArtifactReferenceCount 统计仍引用指定 Artifact 的对话消息数。使用 Go
// 解析薄信封中的 JSON 数组，不依赖部署环境是否编译 SQLite JSON 扩展。
func (s *Store) ArtifactReferenceCount(id string) (int, error) {
	rows, err := s.db.Query(`SELECT artifact_refs FROM chat_messages`)
	if err != nil {
		return 0, fmt.Errorf("查询 Artifact 引用失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return 0, fmt.Errorf("扫描 Artifact 引用失败: %w", err)
		}
		var refs []string
		if err := json.Unmarshal([]byte(encoded), &refs); err != nil {
			return 0, fmt.Errorf("解析消息 Artifact 引用失败: %w", err)
		}
		for _, ref := range refs {
			if ref == id {
				count++
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历 Artifact 引用失败: %w", err)
	}
	return count, nil
}

// DeleteArtifact 删除一个产物：先删元数据行（引用），再删内容本体。
// 为什么这个顺序（与 PutArtifact 相反）：引用先消失，读取方永不会拿到
// 悬空引用；内容文件删除失败只留孤儿文件（可后续清理），不影响读取一致性。
// 产物不存在时返回 ErrNotFound（按影响行数判定）。
func (s *Store) DeleteArtifact(id string) error {
	res, err := s.db.Exec(`DELETE FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除产物元数据 %q 失败: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取删除产物 %q 的影响行数失败: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if err := os.Remove(s.artifactPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// 元数据已删（引用已消失），内容文件删除失败只留孤儿文件，记日志不判失败。
		s.logger.WithError(err).Warn("删除产物内容文件失败", "artifact_id", id)
	}
	return nil
}

// artifactPath 返回产物内容文件的路径（artifactDir/<id>）。
func (s *Store) artifactPath(id string) string {
	return filepath.Join(s.artifactDir, id)
}

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

package robot

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"io"
	"reflect"
)

// 发布身份比较包内路径和内容，忽略压缩时间戳、条目顺序和压缩方式。
// 这样重新打包相同源码仍可重复安装，真正修改同名版本则保持明确冲突。
func sameSkillArchive(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	a, err := skillArchiveFiles(left)
	if err != nil {
		return false
	}
	b, err := skillArchiveFiles(right)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func skillArchiveFiles(data []byte) (map[string][32]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string][32]byte{}
	for _, file := range z.File {
		if file.FileInfo().IsDir() {
			continue
		}
		r, err := file.Open()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(h, r)
		_ = r.Close()
		if err != nil {
			return nil, err
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		files[file.Name] = sum
	}
	return files, nil
}

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

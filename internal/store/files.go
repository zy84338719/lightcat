package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"
)

// 文件类型（TR-069 Download 的 FileType 按它映射，见 cwmp.FileTypeOf）。
const (
	FileTypeFirmware = "firmware" // 1 Firmware Upgrade Image
	FileTypeWeb      = "web"      // 2 Web Content
	FileTypeConfig   = "config"   // 3 Vendor Configuration File
)

// FileTypes 是允许的类型清单（表单校验用）。
var FileTypes = []string{FileTypeFirmware, FileTypeWeb, FileTypeConfig}

// File 是文件库里的一份固件/配置（元数据；内容在磁盘上，见 FilesDir）。
type File struct {
	ID        int64
	Name      string // 给人看的名字（上传时的原始文件名）
	FileType  string // firmware / web / config
	URLPath   string // 内置文件服务的路径（/files/<随机名>，服务端生成）
	Size      int64
	SHA256    string
	Version   string // 固件版本（下发前比对设备当前版本用，可空）
	Model     string // 适用型号（可空 = 通用）
	CreatedAt time.Time
}

// CreateFile 登记一份文件。URLPath 必须非空且以 /files/ 开头 ——
// 它由服务端生成（随机十六进制），绝不来自用户输入。
func (s *Store) CreateFile(f *File) (int64, error) {
	if strings.TrimSpace(f.Name) == "" {
		return 0, fmt.Errorf("文件名不能为空")
	}
	if !strings.HasPrefix(f.URLPath, "/files/") || strings.Contains(f.URLPath, "..") {
		return 0, fmt.Errorf("非法的 url_path: %q", f.URLPath)
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO files
		(name, file_type, url_path, size, sha256, version, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Name, f.FileType, f.URLPath, f.Size, f.SHA256, f.Version, f.Model, ts(f.CreatedAt))
	if err != nil {
		return 0, errors.Wrap(err, "登记文件失败")
	}
	if f.ID, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	return f.ID, nil
}

// ListFiles 列出文件库（新上传的在前）。
func (s *Store) ListFiles() ([]File, error) {
	rows, err := s.db.Query(`SELECT id, name, file_type, url_path, size, sha256, version, model, created_at
		FROM files ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		var t string
		if err := rows.Scan(&f.ID, &f.Name, &f.FileType, &f.URLPath, &f.Size,
			&f.SHA256, &f.Version, &f.Model, &t); err != nil {
			return nil, err
		}
		f.CreatedAt = parseTS(t)
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFile 按 ID 取一份文件的元数据。
func (s *Store) GetFile(id int64) (File, error) {
	var f File
	var t string
	err := s.db.QueryRow(`SELECT id, name, file_type, url_path, size, sha256, version, model, created_at
		FROM files WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &f.FileType, &f.URLPath, &f.Size, &f.SHA256, &f.Version, &f.Model, &t)
	if err != nil {
		return File{}, err
	}
	f.CreatedAt = parseTS(t)
	return f, nil
}

// DeleteFile 删一条文件记录（磁盘文件由调用方删 —— store 不碰文件系统）。
func (s *Store) DeleteFile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

// GetFileByURLPath 按对外路径取元数据（内置文件服务的放行校验：
// 只有登记在库里的才给取，磁盘上的孤儿/残留不服务）。
func (s *Store) GetFileByURLPath(urlPath string) (File, error) {
	var f File
	var t string
	err := s.db.QueryRow(`SELECT id, name, file_type, url_path, size, sha256, version, model, created_at
		FROM files WHERE url_path = ?`, urlPath).
		Scan(&f.ID, &f.Name, &f.FileType, &f.URLPath, &f.Size, &f.SHA256, &f.Version, &f.Model, &t)
	if err != nil {
		return File{}, err
	}
	f.CreatedAt = parseTS(t)
	return f, nil
}

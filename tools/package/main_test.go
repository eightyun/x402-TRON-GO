package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTargetsRejectUnsupportedOrDuplicate(t *testing.T) {
	for _, value := range []string{"", "windows/amd64", "linux/386", "linux", "linux/amd64,linux/amd64", "../linux/arm64"} {
		if _, e := targets(value); e == nil {
			t.Fatal("无效目标未拒绝")
		}
	}
	got, e := targets("linux/arm64,darwin/amd64")
	if e != nil || len(got) != 2 || got[0].OS != "darwin" {
		t.Fatal("目标排序不稳定")
	}
}

func TestLicenseFilesRequireLicenseAndRefuseSymlinks(t *testing.T) {
	dir := t.TempDir()
	module := moduleLicense{Path: "example.invalid/sdk/v2", Version: "v2.0.0", Dir: dir}
	if _, err := licenseFiles(module); !errors.Is(err, errPackage) {
		t.Fatal("缺少许可证未拒绝")
	}
	if os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("test-license"), 0600) != nil || os.WriteFile(filepath.Join(dir, "NOTICE"), []byte("test-notice"), 0600) != nil {
		t.Fatal("写夹具失败")
	}
	files, err := licenseFiles(module)
	if err != nil || len(files) != 2 || files[0].name != "licenses/example.invalid/sdk/v2@v2.0.0/LICENSE" {
		t.Fatal("许可证打包路径或数量不符", err)
	}
	if os.Symlink(filepath.Join(dir, "LICENSE"), filepath.Join(dir, "COPYING")) != nil {
		t.Fatal("建立夹具链接失败")
	}
	if _, err := licenseFiles(module); err == nil {
		t.Fatal("许可证符号链接未拒绝")
	}
	for _, bad := range []moduleLicense{{Path: "../escape", Version: "v1", Dir: dir}, {Path: "valid", Version: "../escape", Dir: dir}, {Path: "valid", Version: "v1", Dir: dir, Replace: &moduleLicense{}}} {
		if _, err := licenseFiles(bad); err == nil {
			t.Fatal("模块替换/路径未拒绝")
		}
	}
}

func TestBuildEnvironmentOverridesUnsafeGoSettings(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=unexpected")
	t.Setenv("GOWORK", "/unexpected")
	want := map[string]string{"GOFLAGS": "", "GOWORK": "off", "GOENV": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "GOARCH": "arm64"}
	for _, item := range buildEnvironment(map[string]string{"GOARCH": "arm64"}) {
		key, value, ok := strings.Cut(item, "=")
		if expected, found := want[key]; found {
			if !ok || value != expected {
				t.Fatal("构建环境未固定", key)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatal("缺失构建环境覆盖")
	}
}

func TestArchivesAreDeterministicAndContainOnlyDeclaredFiles(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Unix(1700000000, 0)
	items := []entry{{"bundle/bin/payer", []byte("binary"), 0755}, {"bundle/LICENSE", []byte("license"), 0644}}
	a, b := filepath.Join(dir, "a.gz"), filepath.Join(dir, "b.gz")
	if archive(a, items, stamp) != nil || archive(b, []entry{items[1], items[0]}, stamp) != nil {
		t.Fatal("归档失败")
	}
	first, e := os.ReadFile(a)
	if e != nil {
		t.Fatal(e)
	}
	second, e := os.ReadFile(b)
	if e != nil || !bytes.Equal(first, second) {
		t.Fatal("同内容/版本/顺序不同产生不同字节")
	}
	gz, e := gzip.NewReader(bytes.NewReader(first))
	if e != nil {
		t.Fatal(e)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if !h.ModTime.Equal(stamp) || h.Uid != 0 || h.Gid != 0 || h.Typeflag != tar.TypeReg {
			t.Fatal("归档泄漏本机用户/时间或文件类型")
		}
		names = append(names, h.Name)
	}
	if len(names) != 2 || names[0] != "bundle/LICENSE" || names[1] != "bundle/bin/payer" {
		t.Fatal("归档文件超出白名单或排序不稳")
	}
	if archive(a, items, stamp) == nil {
		t.Fatal("覆盖已有归档")
	}
}

func TestArchiveRejectsTraversalAndDuplicateNames(t *testing.T) {
	for _, items := range [][]entry{{{"../secret", nil, 0644}}, {{"/secret", nil, 0644}}, {{"a/../secret", nil, 0644}}, {{"x", nil, 0644}, {"x", nil, 0644}}} {
		file := filepath.Join(t.TempDir(), "archive.gz")
		if archive(file, items, time.Unix(1, 0)) == nil {
			t.Fatal("不安全路径/重复项未拒绝")
		}
		if _, e := os.Stat(file); !os.IsNotExist(e) {
			t.Fatal("拒绝之前已创建文件")
		}
	}
}

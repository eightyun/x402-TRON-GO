// package生成未发布的测试网示例包，不创建tag/release，不读取钱包或运行配置。
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}
type entry struct {
	name string
	data []byte
	mode int64
}
type artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Revision   string     `json:"revision"`
	GoVersion  string     `json:"goVersion"`
	CommitTime time.Time  `json:"commitTime"`
	Scope      string     `json:"scope"`
	Targets    []target   `json:"targets"`
	Artifacts  []artifact `json:"artifacts"`
}

var errPackage = errors.New("打包未完成：要求干净源码、可信Go工具链和仓库外的新输出目录")

func main() {
	output := flag.String("output", "", "仓库外的绝对输出父目录")
	matrix := flag.String("targets", "darwin/amd64,darwin/arm64,linux/amd64,linux/arm64", "测试网示例构建目标")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if flag.NArg() != 0 || run(ctx, *output, *matrix) != nil {
		fmt.Fprintln(os.Stderr, errPackage)
		os.Exit(1)
	}
}

func command(ctx context.Context, root string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Dir = root
	if args[0] == "go" {
		c.Env = buildEnvironment(nil)
	}
	b, e := c.Output()
	if e != nil {
		return nil, errPackage
	}
	return b, nil
}

func buildEnvironment(extra map[string]string) []string {
	overrides := map[string]string{"CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOENV": "off", "GOFLAGS": "", "GOEXPERIMENT": "", "GOAMD64": "v1", "GOARM64": "v8.0"}
	for key, value := range extra {
		overrides[key] = value
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, ok := overrides[key]; !ok {
			env = append(env, item)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

type moduleLicense struct {
	Path, Version, Dir string
	Main               bool
	Replace            *moduleLicense
}

func licenseFiles(module moduleLicense) ([]entry, error) {
	if module.Replace != nil || module.Path == "" || path.Clean(module.Path) != module.Path || strings.HasPrefix(module.Path, "/") || strings.HasPrefix(module.Path, "../") || module.Version == "" || strings.ContainsAny(module.Version, "/\\") {
		return nil, errPackage
	}
	var items []entry
	found := false
	names, err := os.ReadDir(module.Dir)
	if err != nil {
		return nil, errPackage
	}
	for _, file := range names {
		name := file.Name()
		if !regexp.MustCompile(`(?i)^(LICENSE|COPYING|NOTICE)([._-].*)?$`).MatchString(name) {
			continue
		}
		filename := filepath.Join(module.Dir, name)
		info, err := os.Lstat(filename)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
			return nil, errPackage
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, errPackage
		}
		items = append(items, entry{"licenses/" + module.Path + "@" + module.Version + "/" + name, data, 0644})
		if strings.HasPrefix(strings.ToUpper(name), "LICENSE") || strings.HasPrefix(strings.ToUpper(name), "COPYING") {
			found = true
		}
	}
	if !found {
		return nil, errPackage
	}
	return items, nil
}

func licenses(ctx context.Context, root string, programs []string) ([]entry, error) {
	args := []string{"go", "list", "-mod=readonly", "-deps", "-json"}
	for _, name := range programs {
		args = append(args, "./examples/"+name)
	}
	data, err := command(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	modules := map[string]moduleLicense{}
	for {
		var pkg struct{ Module *moduleLicense }
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errPackage
		}
		if pkg.Module != nil && !pkg.Module.Main {
			modules[pkg.Module.Path] = *pkg.Module
		}
	}
	var items []entry
	for _, module := range modules {
		files, err := licenseFiles(module)
		if err != nil {
			return nil, err
		}
		items = append(items, files...)
	}
	goRoot, err := command(ctx, root, "go", "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	files, err := licenseFiles(moduleLicense{Path: "go", Version: "toolchain", Dir: strings.TrimSpace(string(goRoot))})
	if err != nil {
		return nil, err
	}
	items = append(items, files...)
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	return items, nil
}

func targets(value string) ([]target, error) {
	seen := map[string]bool{}
	var result []target
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(item, "/")
		if len(parts) != 2 || (parts[0] != "linux" && parts[0] != "darwin") || (parts[1] != "amd64" && parts[1] != "arm64") || seen[item] {
			return nil, errPackage
		}
		seen[item] = true
		result = append(result, target{parts[0], parts[1]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OS+result[i].Arch < result[j].OS+result[j].Arch })
	return result, nil
}

func archive(filename string, items []entry, stamp time.Time) error {
	items = append([]entry(nil), items...)
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	for i, e := range items {
		if e.name == "" || e.name == "." || path.Clean(e.name) != e.name || strings.HasPrefix(e.name, "/") || e.name == ".." || strings.HasPrefix(e.name, "../") || (i > 0 && e.name == items[i-1].name) {
			return errPackage
		}
	}
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return errPackage
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range items {
		if tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), ModTime: stamp.UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}) != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			return errPackage
		}
		if _, err := tw.Write(e.data); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			return errPackage
		}
	}
	a, b, c := tw.Close(), gz.Close(), f.Close()
	if a != nil || b != nil || c != nil {
		return errPackage
	}
	return nil
}

func write(filename string, b []byte) error {
	f, e := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if e != nil {
		return errPackage
	}
	n, w := f.Write(b)
	s := f.Sync()
	c := f.Close()
	if n != len(b) || w != nil || s != nil || c != nil {
		return errPackage
	}
	return nil
}

func run(ctx context.Context, output, matrix string) error {
	targets, err := targets(matrix)
	if err != nil || !filepath.IsAbs(output) || filepath.Clean(output) != output || output == string(filepath.Separator) {
		return errPackage
	}
	b, err := command(ctx, "", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root := strings.TrimSpace(string(b))
	rel, err := filepath.Rel(root, output)
	if err != nil || !(rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errPackage
	}
	clean := func() bool {
		b, e := command(ctx, root, "git", "status", "--porcelain", "--untracked-files=normal")
		return e == nil && len(bytes.TrimSpace(b)) == 0
	}
	if !clean() {
		return errPackage
	}
	b, err = command(ctx, root, "git", "rev-parse", "HEAD")
	sha := strings.TrimSpace(string(b))
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		return errPackage
	}
	b, err = command(ctx, root, "git", "show", "-s", "--format=%ct", "HEAD")
	seconds, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || e != nil || seconds <= 0 {
		return errPackage
	}
	stamp := time.Unix(seconds, 0).UTC()
	b, err = command(ctx, root, "go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(b))
	label := "x402-tron-go-" + sha[:12]
	destination := filepath.Join(output, label)
	if _, e := os.Lstat(destination); !errors.Is(e, os.ErrNotExist) {
		return errPackage
	}
	if os.MkdirAll(output, 0755) != nil {
		return errPackage
	}
	work, err := os.MkdirTemp(output, ".x402-package-")
	if err != nil {
		return errPackage
	}
	defer os.RemoveAll(work)
	details := manifest{Revision: sha, GoVersion: version, CommitTime: stamp, Scope: "unaudited-testnet-examples", Targets: targets}
	addArtifact := func(name string) error {
		b, e := os.ReadFile(filepath.Join(work, name))
		if e != nil {
			return errPackage
		}
		sum := sha256.Sum256(b)
		details.Artifacts = append(details.Artifacts, artifact{name, hex.EncodeToString(sum[:])})
		return nil
	}
	// 源码只从提交对象归档，不扫描本机目录，避免把忽略的敏感运行材料打入包。
	source, err := command(ctx, root, "git", "archive", "--format=tar", sha)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(source); err != nil {
		return errPackage
	}
	if zw.Close() != nil {
		return errPackage
	}
	sourceName := label + "-source.tar.gz"
	if write(filepath.Join(work, sourceName), compressed.Bytes()) != nil || addArtifact(sourceName) != nil {
		return errPackage
	}
	programs := []string{"deployment", "facilitator", "payer", "server"}
	licenseEntries, err := licenses(ctx, root, programs)
	if err != nil {
		return err
	}
	for _, target := range targets {
		bundle := label + "-" + target.OS + "-" + target.Arch
		bin := filepath.Join(work, "bin")
		if os.Mkdir(bin, 0700) != nil {
			return errPackage
		}
		args := []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=true", "-ldflags=-s -w -buildid=", "-o", bin + string(filepath.Separator)}
		for _, name := range programs {
			args = append(args, "./examples/"+name)
		}
		build := exec.CommandContext(ctx, "go", args...)
		build.Dir = root
		build.Env = buildEnvironment(map[string]string{"GOOS": target.OS, "GOARCH": target.Arch})
		if build.Run() != nil {
			return errPackage
		}
		var items []entry
		for _, license := range licenseEntries {
			items = append(items, entry{bundle + "/" + license.name, license.data, license.mode})
		}
		for _, name := range programs {
			b, e := os.ReadFile(filepath.Join(bin, name))
			if e != nil {
				return errPackage
			}
			items = append(items, entry{bundle + "/bin/" + name, b, 0755})
		}
		for name, source := range map[string]string{"LICENSE": "LICENSE", "THIRD_PARTY_NOTICES.md": "THIRD_PARTY_NOTICES.md", "README.md": "README.md", "README.en.md": "README.en.md", ".env.example": ".env.example", "schema.sql": "exact/postgres/schema.sql", "upgrade_confirmation.sql": "exact/postgres/upgrade_confirmation.sql"} {
			b, e := command(ctx, root, "git", "show", sha+":"+source)
			if e != nil {
				return e
			}
			items = append(items, entry{bundle + "/" + name, b, 0644})
		}
		metadata, _ := json.MarshalIndent(struct {
			Revision, GoVersion, Scope, OS, Arch string
			CommitTime                           time.Time
		}{sha, version, details.Scope, target.OS, target.Arch, stamp}, "", "  ")
		items = append(items, entry{bundle + "/metadata.json", metadata, 0644})
		name := bundle + ".tar.gz"
		if archive(filepath.Join(work, name), items, stamp) != nil || addArtifact(name) != nil {
			return errPackage
		}
		// 仅删除工具自己创建的唯一临时构建目录，不触碰源码或已有包。
		if os.RemoveAll(bin) != nil {
			return errPackage
		}
		fmt.Println("built", target.OS+"/"+target.Arch)
	}
	if !clean() {
		return errPackage
	}
	sort.Slice(details.Artifacts, func(i, j int) bool { return details.Artifacts[i].Name < details.Artifacts[j].Name })
	b, _ = json.MarshalIndent(details, "", "  ")
	if write(filepath.Join(work, "manifest.json"), append(b, '\n')) != nil {
		return errPackage
	}
	if addArtifact("manifest.json") != nil {
		return errPackage
	}
	sort.Slice(details.Artifacts, func(i, j int) bool { return details.Artifacts[i].Name < details.Artifacts[j].Name })
	var sums strings.Builder
	for _, a := range details.Artifacts {
		_, _ = fmt.Fprintf(&sums, "%s  %s\n", a.SHA256, a.Name)
	}
	if write(filepath.Join(work, "SHA256SUMS"), []byte(sums.String())) != nil {
		return errPackage
	}
	if _, e := os.Lstat(destination); !errors.Is(e, os.ErrNotExist) {
		return errPackage
	}
	if os.Rename(work, destination) != nil {
		return errPackage
	}
	fmt.Println("packages", destination)
	return nil
}

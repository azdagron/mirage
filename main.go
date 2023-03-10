package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zeebo/errs"
)

func main() {
	opts := new(Options)

	fs := flag.NewFlagSet("mirage", flag.ExitOnError)
	fs.StringVar(&opts.DstModule, "dst-module", "", "The destination module name (autodetected via destination go.mod if unset)")
	fs.BoolVar(&opts.LocalImports, "local-imports", true, "Fix up imports to treat the destination module as local imports")
	fs.Parse(os.Args[1:])
	args := fs.Args()

	switch {
	case len(args) < 1:
		badUsage("missing source package (SRCDIR)")
	case len(args) < 2:
		badUsage("missing destination directory (DSTDIR)")
	}

	srcDir := args[0]
	dstDir := args[1]

	if err := run(dstDir, srcDir, opts); err != nil {
		log.Fatalf("%+v", err)
	}
}

func badUsage(why string) {
	fmt.Fprintf(os.Stderr, "%s\n", why)
	fmt.Fprintln(os.Stderr, "mirage [-dst-module=DSTMODULE] [-local-imports=<true/false>] SRCDIR DSTDIR")
	os.Exit(1)
}

type Options struct {
	DstModule    string
	LocalImports bool
}

func run(dstDir, srcDir string, opts *Options) error {
	log.Println("Building work...")
	work, err := getWork(dstDir, srcDir, opts)
	if err != nil {
		return err
	}

	return doWork(work, opts)
}

func doWork(work *Work, opts *Options) error {
	log.Println("Cleaning destination...")
	if err := cleanDst(work.DstDir); err != nil {
		return fmt.Errorf("failed to clean destination: %w", err)
	}

	log.Println("Preparing go.mod...")
	if err := copyOtherFile(work.SrcGoMod, work.DstGoMod); err != nil {
		return fmt.Errorf("failed to copy go.mod: %v", err)
	}
	if err := execInDir(work.DstDir, "go", "mod", "edit", "-module", work.DstModule); err != nil {
		return fmt.Errorf("failed to rename destination module: %w", err)
	}

	// Prepare package name replacements
	log.Println("Copying Go source files...")
	r := strings.NewReplacer(work.PackageReplacements...)
	localModule := ""
	if opts.LocalImports {
		localModule = work.DstModule
	}
	for src, dst := range work.GoFiles {
		if err := copyGoFile(src, dst, r, localModule); err != nil {
			return err
		}
	}

	log.Println("Copying non-Go source files...")
	for src, dst := range work.OtherFiles {
		if err := copyOtherFile(src, dst); err != nil {
			return err
		}
	}

	log.Println("Tidying...")
	if err := execInDir(work.DstDir, "go", "mod", "tidy"); err != nil {
		return fmt.Errorf("failed to tidy: %w", err)
	}

	log.Println("Done.")
	return nil
}

type Work struct {
	SrcDir              string
	SrcGoMod            string
	SrcImportPath       string
	DstDir              string
	DstGoMod            string
	DstModule           string
	GoFiles             map[string]string
	OtherFiles          map[string]string
	PackageReplacements []string
}

func (w *Work) addCopies(srcDir, dstDir string, files []string) {
	for _, file := range files {
		src := filepath.Join(srcDir, file)
		dst := filepath.Join(dstDir, file)
		if filepath.Ext(file) == ".go" {
			w.GoFiles[src] = dst
		} else {
			w.OtherFiles[src] = dst
		}
	}
}

func (w *Work) addPackageReplacement(srcPkg, dstPkg string) {
	w.PackageReplacements = append(w.PackageReplacements, strconv.Quote(srcPkg), strconv.Quote(dstPkg))
}

func getWork(dstDir, srcDir string, opts *Options) (_ *Work, err error) {
	work := &Work{
		SrcDir:     srcDir,
		DstDir:     dstDir,
		DstGoMod:   filepath.Join(dstDir, "go.mod"),
		GoFiles:    make(map[string]string),
		OtherFiles: make(map[string]string),
	}

	work.DstModule, err = getModulePath(dstDir)
	if err != nil && fileExists(filepath.Join(dstDir, "go.mod")) {
		return nil, fmt.Errorf("failed to get package info for destination: %w", err)
	}

	switch {
	case opts.DstModule != "":
		work.DstModule = opts.DstModule
	case work.DstModule == "":
		return nil, errors.New("no destination module available; use --dst-module or create go.mod at the destination")
	}

	srcInfo, err := getPackageInfo(srcDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get package info for source: %w", err)
	}
	work.SrcGoMod = srcInfo.Module.GoMod
	work.SrcImportPath = srcInfo.ImportPath
	work.addPackageReplacement(work.SrcImportPath, work.DstModule)
	work.addCopies(srcDir, dstDir, srcInfo.AllFiles())

	next := make(map[string]struct{})
	for _, dep := range srcInfo.Deps {
		next[dep] = struct{}{}
	}

	done := make(map[string]struct{})

	// Figure out which deps are in-module and need to be copied
	prefix := srcInfo.Module.Path + "/"

	for len(next) > 0 {
		deps := next
		next = make(map[string]struct{})
		for dep := range deps {
			if _, ok := done[dep]; ok {
				continue
			}
			done[dep] = struct{}{}

			suffix, cut := strings.CutPrefix(dep, prefix)
			if !cut {
				continue
			}

			depSrcDir := filepath.Join(srcInfo.Module.Dir, suffix)
			depDstDir := filepath.Join(dstDir, "internal", suffix)

			depInfo, err := getPackageInfo(depSrcDir)
			if err != nil {
				return nil, fmt.Errorf("failed to get package info for dependency package %q: %w", suffix, err)
			}

			work.addPackageReplacement(depInfo.ImportPath, path.Join(work.DstModule, "internal", suffix))
			work.addCopies(depSrcDir, depDstDir, depInfo.AllFiles())
		}
	}

	return work, nil
}

func copyOtherFile(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("failed to ensure destination directory exists: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer func() {
		_ = src.Close()
	}()

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer func() {
		_ = dst.Close()
	}()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy file %w", err)
	}

	if err := dst.Close(); err != nil {
		return fmt.Errorf("failed to close destination file: %w", err)
	}

	return nil
}

func copyGoFile(srcPath, dstPath string, r *strings.Replacer, localModule string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return errs.Wrap(err)
	}

	code := new(bytes.Buffer)
	if _, err := r.WriteString(code, string(data)); err != nil {
		return errs.Wrap(err)
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("failed to ensure destination directory exists: %w", err)
	}

	if err := os.WriteFile(dstPath, code.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write destination file: %w", err)
	}

	args := []string{"goimports", "-w"}
	if localModule != "" {
		args = append(args, "-local", localModule)
	}
	args = append(args, filepath.Base(dstPath))

	if err := execInDir(filepath.Dir(dstPath), "goimports", args...); err != nil {
		return err
	}

	return nil
}

type packageInfo struct {
	ImportPath string
	Module     struct {
		Path  string
		Dir   string
		GoMod string
	}

	GoFiles           []string
	CgoFiles          []string
	CompiledGoFiles   []string
	IgnoredGoFiles    []string
	IgnoredOtherFiles []string
	CFiles            []string
	CXXFiles          []string
	MFiles            []string
	HFiles            []string
	FFiles            []string
	SFiles            []string
	SwigFiles         []string
	SwigCXXFiles      []string
	SysoFiles         []string
	EmbedFiles        []string

	Deps []string
}

func (info *packageInfo) AllFiles() (all []string) {
	all = append(all, info.GoFiles...)
	all = append(all, info.CgoFiles...)
	all = append(all, info.CompiledGoFiles...)
	all = append(all, info.IgnoredGoFiles...)
	all = append(all, info.IgnoredOtherFiles...)
	all = append(all, info.CFiles...)
	all = append(all, info.CXXFiles...)
	all = append(all, info.MFiles...)
	all = append(all, info.HFiles...)
	all = append(all, info.FFiles...)
	all = append(all, info.SFiles...)
	all = append(all, info.SwigFiles...)
	all = append(all, info.SwigCXXFiles...)
	all = append(all, info.SysoFiles...)
	all = append(all, info.EmbedFiles...)
	return all
}

func getModulePath(dir string) (string, error) {
	info := &struct {
		Module struct {
			Path string
		}
	}{}
	if err := execInDirAndParseJSON(dir, info, "go", "mod", "edit", "-json"); err != nil {
		return "", err
	}
	return info.Module.Path, nil
}

func getPackageInfo(dir string) (*packageInfo, error) {
	info := new(packageInfo)
	if err := execInDirAndParseJSON(dir, info, "go", "list", "-json", "."); err != nil {
		return nil, err
	}
	return info, nil
}

func cleanDst(dir string) error {
	// Remove go src files, skipping any directory with a leading dot
	if err := filepath.Walk(dir, filepath.WalkFunc(func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return errs.Wrap(walkErr)
		}

		// Skip files and folders beginning with dot
		if strings.HasPrefix(path, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Don't try and remove directories in this step.
		if info.IsDir() {
			return nil
		}

		// Skip non-go files
		if filepath.Ext(path) != ".go" {
			return nil
		}
		return errs.Wrap(os.Remove(path))
	})); err != nil {
		return errs.Wrap(err)
	}

	// Now remove empty directories
	if err := filepath.Walk(dir, filepath.WalkFunc(func(path string, info fs.FileInfo, walkErr error) error {
		switch {
		case walkErr != nil:
			return errs.Wrap(walkErr)
		case !info.IsDir():
			return nil
		}

		if children, err := os.ReadDir(path); err != nil {
			return errs.Wrap(err)
		} else if len(children) > 0 {
			return nil
		}
		return errs.Wrap(os.Remove(path))
	})); err != nil {
		return errs.Wrap(err)
	}

	return nil
}

func execInDir(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}
	return nil
}

func execInDirAndParseJSON(dir string, obj interface{}, name string, args ...string) error {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), obj); err != nil {
		return fmt.Errorf("failed to unmarshal package info: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.Mode()&os.ModeType == 0
}

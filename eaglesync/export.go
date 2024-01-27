package eaglesync

import (
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cockroachdb/errors"
	"github.com/djherbis/times"
	"github.com/schollz/progressbar/v3"
	"github.com/sourcegraph/conc/pool"
	"github.com/spf13/afero"
)

type Library struct {
	BaseDir string
	fs      afero.Fs
}

func NewLibrary(baseDir string, fs afero.Fs) *Library {
	return &Library{
		BaseDir: baseDir,
		fs:      fs,
	}
}

type ExportOption struct {
	// Overwrite the existing file
	Overwrite bool

	// Force clean up the destination directory before export
	Force bool

	// Bar cli progress bar
	Bar *progressbar.ProgressBar

	// GroupBySmartFolder export group by smart folder
	GroupBySmartFolder bool
}

func (e *Library) Export(outputDir string, option ExportOption) error {
	if option.Force {
		err := e.fs.RemoveAll(outputDir)
		if err != nil {
			return errors.Wrapf(err, "delete directory '%v' failed", outputDir)
		}
	}

	var mtimeMap Mtime
	err := parseJsonFile(filepath.Join(e.BaseDir, "mtime.json"), &mtimeMap)
	if err != nil {
		return err
	}

	var libraryMetadata LibraryInfo
	err = parseJsonFile(filepath.Join(e.BaseDir, "metadata.json"), &libraryMetadata)
	if err != nil {
		return err
	}

	filter := NewFolderFilter(&libraryMetadata)

	count, ok := mtimeMap["all"]
	if !ok {
		return errors.New("field 'all' not exists")
	}

	bar := option.Bar
	if bar != nil {
		bar.ChangeMax64(count)
		defer func() { _ = bar.Finish() }()
	}

	p := pool.New().WithErrors().WithMaxGoroutines(runtime.NumCPU())
	for fileInfoName, mtime := range mtimeMap {
		fileInfoName := fileInfoName
		mtime := mtime

		if fileInfoName == "all" {
			continue
		}

		p.Go(func() error {
			var fileInfo FileInfo
			fileMetadataPath := filepath.Join(e.BaseDir, "images", fileInfoName+".info", "metadata.json")
			err = parseJsonFile(fileMetadataPath, &fileInfo)
			if err != nil {
				return err
			}

			if fileInfo.IsDeleted {
				return nil
			}

			infoDir := filepath.Join(e.BaseDir, "images", fileInfoName+".info")
			fileName := fileInfo.Name + "." + fileInfo.Ext
			src := filepath.Join(infoDir, fileName)

			var dst string
			if option.GroupBySmartFolder {
				var category string
				category, err = filter.Evaluate(&fileInfo)
				if err != nil {
					return err
				}

				if category == "" {
					dst = filepath.Join(outputDir, "uncategorized", fileName)
				} else {
					dst = filepath.Join(outputDir, category, fileName)
				}
			} else {
				dst = filepath.Join(outputDir, fileName)
			}

			return e.copyFile(src, dst, mtime, &option)
		})
	}
	return p.Wait()
}

func (e *Library) copyFile(src string, dst string, fileMtime int64, option *ExportOption) error {
	// TODO: src file is always in the OS fs or not?
	srcFile, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "open src file failed")
	}
	defer func() { _ = srcFile.Close() }()

	srcStat, err := times.StatFile(srcFile)
	if err != nil {
		return errors.Wrap(err, "stat src file failed")
	}

	_ = e.fs.MkdirAll(filepath.Dir(dst), 0755)
	dstFile, err := e.fs.OpenFile(dst, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0655)
	if err != nil {
		return errors.Wrap(err, "open dst file failed")
	}
	defer func() { _ = dstFile.Close() }()

	dstStat, err := dstFile.Stat()
	if err != nil {
		return errors.Wrap(err, "stat dst file failed")
	}

	if srcStat.ModTime() != dstStat.ModTime() || fileMtime != dstStat.ModTime().UnixMilli() || option.Overwrite {
		var writer io.Writer
		if option.Bar != nil {
			writer = io.MultiWriter(dstFile, option.Bar)
		} else {
			writer = dstFile
		}
		_, err = io.Copy(writer, srcFile)
		if err != nil {
			return errors.Wrap(err, "copy file failed")
		}
		err = e.fs.Chtimes(dst, srcStat.AccessTime(), srcStat.ModTime())
		if err != nil {
			return errors.Wrapf(err, "chtimes failed, path: %v", dst)
		}
	} else {
		return errors.Wrap(err, "stat dst file failed")
	}

	return nil
}

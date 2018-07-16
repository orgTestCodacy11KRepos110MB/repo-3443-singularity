// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package underlay

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/singularityware/singularity/src/pkg/sylog"

	"github.com/singularityware/singularity/src/pkg/util/fs/layout"
	"github.com/singularityware/singularity/src/pkg/util/fs/mount"
)

const underlayDir = "/underlay"

type pathLen struct {
	path string
	len  uint16
}

// Underlay layer manager
type Underlay struct {
	session *layout.Session
}

// New creates and returns an overlay layer manager
func New() *Underlay {
	return &Underlay{}
}

// Add adds required directory in session layout
func (u *Underlay) Add(session *layout.Session) error {
	u.session = session
	if err := u.session.AddDir(underlayDir); err != nil {
		return err
	}
	return nil
}

// Prepare registers hook function to be executed during mount phase
func (u *Underlay) Prepare(system *mount.System) error {
	if err := system.RunBeforeTag(mount.PreLayerTag, u.createUnderlay); err != nil {
		return err
	}
	return nil
}

func (u *Underlay) createUnderlay(system *mount.System) error {
	points := system.Points.GetByTag(mount.RootfsTag)
	if len(points) != 1 {
		return fmt.Errorf("no root fs image found")
	}
	return u.createLayer(points[0].Destination, system)
}

// createLayer creates underlay layer based on content of root filesystem
func (u *Underlay) createLayer(rootFsPath string, system *mount.System) error {
	st := new(syscall.Stat_t)
	points := system.Points
	createdPath := make([]pathLen, 0)

	sessionDir := u.session.Path()
	for _, tag := range mount.GetTagList() {
		for _, point := range points.GetByTag(tag) {
			if strings.HasPrefix(point.Destination, sessionDir) {
				continue
			}
			if err := syscall.Stat(rootFsPath+point.Destination, st); err == nil {
				continue
			}
			if err := syscall.Stat(point.Source, st); err != nil {
				sylog.Warningf("skipping mount of %s: %s", point.Source, err)
				continue
			}
			dst := underlayDir + point.Destination
			switch st.Mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				if err := u.session.AddDir(dst); err != nil {
					return err
				}
			default:
				if err := u.session.AddFile(dst, nil); err != nil {
					return err
				}
			}
			dst, _ = u.session.GetPath(dst)
			if err := system.Points.AddBind(mount.PreLayerTag, point.Source, dst, syscall.MS_BIND); err != nil {
				return fmt.Errorf("can't add bind mount point: %s", err)
			}
			createdPath = append(createdPath, pathLen{path: point.Destination, len: uint16(strings.Count(point.Destination, "/"))})
		}
	}

	sort.SliceStable(createdPath, func(i, j int) bool { return createdPath[i].len < createdPath[j].len })

	for _, pl := range createdPath {
		splitted := strings.Split(filepath.Dir(pl.path), string(os.PathSeparator))
		l := len(splitted)
		p := ""
		for i := 1; i < l; i++ {
			s := splitted[i : i+1][0]
			p += "/" + s
			if s != "" {
				if _, err := u.session.GetPath(p); err != nil {
					if err := u.session.AddDir(p); err != nil {
						return err
					}
				}
				if err := u.duplicateDir(p, system); err != nil {
					return err
				}
			}
		}
	}

	if err := u.duplicateDir("/", system); err != nil {
		return err
	}

	path, _ := u.session.GetPath(underlayDir)
	err := system.Points.AddBind(mount.LayerTag, path, u.session.FinalPath(), syscall.MS_BIND|syscall.MS_REC)
	if err != nil {
		return err
	}
	return u.session.Update()
}

func (u *Underlay) duplicateDir(dir string, system *mount.System) error {
	path := filepath.Clean(u.session.RootFsPath() + dir)
	files, err := ioutil.ReadDir(path)
	if err != nil {
		// directory doesn't exists, nothing to duplicate
		return nil
	}
	for _, file := range files {
		dst := filepath.Join(underlayDir+dir, file.Name())
		src := filepath.Join(path, file.Name())

		// no error means entry is already created
		if _, err := u.session.GetPath(dst); err == nil {
			continue
		}
		if file.IsDir() {
			if err := u.session.AddDir(dst); err != nil {
				return fmt.Errorf("can't add directory %s to underlay: %s", dst, err)
			}
			dst, _ = u.session.GetPath(dst)
			if err := system.Points.AddBind(mount.PreLayerTag, src, dst, syscall.MS_BIND); err != nil {
				return fmt.Errorf("can't add bind mount point: %s", err)
			}
		} else if file.Mode()&os.ModeSymlink != 0 {
			tgt, err := os.Readlink(src)
			if err != nil {
				return fmt.Errorf("can't read symlink information for %s: %s", src, err)
			}
			if err := u.session.AddSymlink(dst, tgt); err != nil {
				return fmt.Errorf("can't add symlink: %s", err)
			}
		} else {
			if err := u.session.AddFile(dst, nil); err != nil {
				return fmt.Errorf("can't add directory %s to underlay: %s", dst, err)
			}
			dst, _ = u.session.GetPath(dst)
			if err := system.Points.AddBind(mount.PreLayerTag, src, dst, syscall.MS_BIND); err != nil {
				return fmt.Errorf("can't add bind mount point: %s", err)
			}
		}
	}
	return nil
}

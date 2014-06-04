package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"github.com/GeertJohan/go.rice/embedded"
	"github.com/akavel/rsrc/coff"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

type sizedBytes []byte

func (s sizedBytes) Size() int64 {
	return int64(len(s))
}

var tmplEmbeddedSysoHelper *template.Template

func init() {
	var err error
	tmplEmbeddedSysoHelper, err = template.New("embeddedSysoHelper").Parse(`package {{.Package}}

// extern char _bricebox_{{.Symname}}[], _ericebox_{{.Symname}};
// int get_{{.Symname}}_length() {
// 	return &_ericebox_{{.Symname}} - _bricebox_{{.Symname}};
// }
import "C"
import (
	"bytes"
	"encoding/gob"
	"github.com/GeertJohan/go.rice/embedded"
	"unsafe"
)

// func get_{{.Symname}}() []byte {
// 	ptr := unsafe.Pointer(&C._bricebox_{{.Symname}})
// 	bts := C.GoBytes(ptr, C.get_{{.Symname}}_length())
// 	return bts
// }

func init() {
	ptr := unsafe.Pointer(&C._bricebox_{{.Symname}})
	bts := C.GoBytes(ptr, C.get_{{.Symname}}_length())
	embeddedBox := &embedded.EmbeddedBox{}
	err := gob.NewDecoder(bytes.NewReader(bts)).Decode(embeddedBox)
	if err != nil {
		panic("error decoding embedded box: "+err.Error())
	}
	embeddedBox.Link()
	embedded.RegisterEmbeddedBox(embeddedBox.Name, embeddedBox)
}`)
	if err != nil {
		panic("could not parse template embeddedSysoHelper: " + err.Error())
	}
}

type embeddedSysoHelperData struct {
	Package string
	Symname string
}

func operationEmbedSyso(pkg *build.Package) {

	regexpSynameReplacer := regexp.MustCompile(`[^a-z0-9_]`)

	boxMap := findBoxes(pkg)

	// notify user when no calls to rice.FindBox are made (is this an error and therefore os.Exit(1) ?
	if len(boxMap) == 0 {
		fmt.Println("no calls to rice.FindBox() found")
		return
	}

	verbosef("\n")

	for boxname := range boxMap {
		// find path and filename for this box
		boxPath := filepath.Join(pkg.Dir, boxname)
		boxFilename := strings.Replace(boxname, "/", "-", -1)
		boxFilename = strings.Replace(boxFilename, "..", "back", -1)

		// verbose info
		verbosef("embedding box '%s'\n", boxname)
		verbosef("\tto file %s\n", boxFilename)

		// create box datastructure (used by template)
		box := &embedded.EmbeddedBox{
			Name:      boxname,
			Time:      time.Now(),
			EmbedType: embedded.EmbedTypeSyso,
			Files:     make(map[string]*embedded.EmbeddedFile),
			Dirs:      make(map[string]*embedded.EmbeddedDir),
		}

		// fill box datastructure with file data
		filepath.Walk(boxPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				fmt.Printf("error walking box: %s\n", err)
				os.Exit(1)
			}

			filename := strings.TrimPrefix(path, boxPath)
			filename = strings.Replace(filename, "\\", "/", -1)
			filename = strings.TrimPrefix(filename, "/")
			if info.IsDir() {
				embeddedDir := &embedded.EmbeddedDir{
					Filename:   filename,
					DirModTime: info.ModTime(),
				}
				verbosef("\tincludes dir: '%s'\n", embeddedDir.Filename)
				box.Dirs[embeddedDir.Filename] = embeddedDir

				// add tree entry (skip for root, it'll create a recursion)
				if embeddedDir.Filename != "" {
					pathParts := strings.Split(embeddedDir.Filename, "/")
					parentDir := box.Dirs[strings.Join(pathParts[:len(pathParts)-1], "/")]
					parentDir.ChildDirs = append(parentDir.ChildDirs, embeddedDir)
				}
			} else {
				embeddedFile := &embedded.EmbeddedFile{
					Filename:    filename,
					FileModTime: info.ModTime(),
					Content:     "",
				}
				verbosef("\tincludes file: '%s'\n", embeddedFile.Filename)
				contentBytes, err := ioutil.ReadFile(path)
				if err != nil {
					fmt.Printf("error reading file content while walking box: %s\n", err)
					os.Exit(1)
				}
				embeddedFile.Content = string(contentBytes)
				box.Files[embeddedFile.Filename] = embeddedFile
			}
			return nil
		})

		// encode embedded box to gob file
		boxGobBuf := &bytes.Buffer{}
		err := gob.NewEncoder(boxGobBuf).Encode(box)
		if err != nil {
			fmt.Printf("error encoding box to gob: %v\n", err)
			os.Exit(1)
		}

		// write coff
		symname := regexpSynameReplacer.ReplaceAllString(boxname, "_")
		createCoffSyso(boxname, symname, "386", boxGobBuf.Bytes())
		createCoffSyso(boxname, symname, "amd64", boxGobBuf.Bytes())

		// write go
		sysoHelperData := embeddedSysoHelperData{
			Package: pkg.Name,
			Symname: symname,
		}
		fileSysoHelper, err := os.Create(boxFilename + ".rice-box.go")
		if err != nil {
			fmt.Printf("error creating syso helper: %v\n", err)
			os.Exit(1)
		}
		err = tmplEmbeddedSysoHelper.Execute(fileSysoHelper, sysoHelperData)
		if err != nil {
			fmt.Printf("error executing tmplEmbeddedSysoHelper: %v\n", err)
			os.Exit(1)
		}
	}
}

func createCoffSyso(boxFilename string, symname string, arch string, data []byte) {
	boxCoff := coff.NewRDATA()
	switch arch {
	case "386":
	case "amd64":
		boxCoff.FileHeader.Machine = 0x8664
	default:
		panic("invalid arch")
	}
	boxCoff.AddData("_bricebox_"+symname, sizedBytes(data))
	boxCoff.AddData("_ericebox_"+symname, io.NewSectionReader(strings.NewReader("\000\000"), 0, 2)) // TODO: why? copied from rsrc, which copied it from as-generated
	boxCoff.Freeze()
	err := writeCoff(boxCoff, boxFilename+"_"+arch+".rice-box.syso")
	if err != nil {
		fmt.Printf("error writing %s coff/.syso: %v\n", arch, err)
		os.Exit(1)
	}
}

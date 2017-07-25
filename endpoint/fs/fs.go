package fs

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/pkg/errors"
	"github.com/pydio/poc/sync/common"
	"github.com/pydio/services/common/proto/tree"
	"github.com/rjeczalik/notify"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/afero"
)

var (
	normalize, denormalize func(string) string
)

const (
	macOS = "darwin"
	win   = "windows"
)

func init() {
	path := func(p string) string {
		return fmt.Sprintf("/%v", strings.TrimLeft(p, common.InternalPathSeparator))
	}

	switch runtime.GOOS {
	case macOS:
		normalize = func(p string) string {
			return string(norm.NFC.Bytes([]byte(path(p))))
		}

		denormalize = func(p string) string {
			return string(norm.NFD.Bytes([]byte(path(p))))
		}
	case win:
		normalize = func(p string) string {
			return strings.Replace(path(p), string(os.PathSeparator),
				common.InternalPathSeparator, -1)
		}

		denormalize = func(p string) string {
			return strings.Replace(path(p), common.InternalPathSeparator,
				string(os.PathSeparator), -1)
		}
	}
}

var (
	// EventTypePut contains the notify events that will cause a put (writer)
	EventTypeAll = []notify.Event{notify.All}
	// EventTypePut contains the notify events that will cause a put (writer)
	EventTypePut = []notify.Event{notify.Create, notify.Write, notify.Rename}
	// EventTypePut contains the notify events that will cause a put (writer)
	EventTypeCreate = []notify.Event{notify.Create}
	// EventTypePut contains the notify events that will cause a put (writer)
	EventTypeWrite = []notify.Event{notify.Write}
	// EventTypePut contains the notify events that will cause a put (writer)
	EventTypeRename = []notify.Event{notify.Rename}
	// EventTypeDelete contains the notify events that will cause a delete (remove)
	EventTypeDelete = []notify.Event{notify.Remove}
	// EventTypeGet contains the notify events that will cause a get (read)
	EventTypeGet = []notify.Event{} // On macOS, FreeBSD, Solaris this is not available.

)

// PipeChan builds a new dynamically sized channel
func PipeChan(capacity int) (inputCh chan notify.EventInfo, outputCh chan notify.EventInfo) {

	// A set of channels which store all elements received from input
	channels := make(chan chan notify.EventInfo, 1000)

	inputCh = make(chan notify.EventInfo, capacity)

	// A goroutine which receives elements from inputCh and creates
	// new channels when needed.
	go func() {
		// Create the first channel
		currCh := make(chan notify.EventInfo, capacity)
		channels <- currCh

		for elem := range inputCh {
			// Prepare next channel with a double capacity when
			// half of the current channel is already filled.
			if len(currCh) >= cap(currCh)/2 {
				close(currCh)
				currCh = make(chan notify.EventInfo, cap(currCh)*2)
				channels <- currCh
			}
			// Prepare next channel with half capacity when
			// current channel is 1/4 filled
			if len(currCh) >= capacity && len(currCh) <= cap(currCh)/4 {
				close(currCh)
				currCh = make(chan notify.EventInfo, cap(currCh)/2)
				channels <- currCh
			}
			// Send element to current channel
			currCh <- elem
		}

		close(currCh)
		close(channels)
	}()

	// Copy elements from infinite channel set to the output
	outputCh = make(chan notify.EventInfo, capacity)
	go func() {
		for {
			currCh, ok := <-channels
			if !ok {
				break
			}
			for v := range currCh {
				outputCh <- v
			}
		}
		close(outputCh)
	}()
	return inputCh, outputCh
}

// Get current time in a predefined format
func now() string {
	TimeFormatFS := "2006-01-02T15:04:05.000Z"
	return time.Now().UTC().Format(TimeFormatFS)
}

// IsEventType checks if the event is of a certain type
func isEventType(eventType []notify.Event, event notify.Event) bool {
	for _, ev := range eventType {
		if event&ev != 0 {
			return true
		}
	}
	return false
}

// CanonicalPath ...
// TODO MOVE IN A fs_windows FILE TO AVOID RUNTIME OS CHECK
func CanonicalPath(path string) string {

	if runtime.GOOS == "windows" {
		// Remove any leading slash/backslash
		path = strings.TrimLeft(path, "/\\")
		p, e := filepath.EvalSymlinks(path)
		if e != nil {
			return path
		} else if p != path {
			// Make sure drive letter is lowerCase
			volume := filepath.VolumeName(p)
			if strings.HasSuffix(volume, ":") {
				p = strings.ToLower(volume) + strings.TrimPrefix(p, volume)
			}
			return p
		}
	}
	return path

}

// Transform an OS notify event to a standard pydio EventInfo
func notifyEventToEventInfo(c *FSClient, event notify.EventInfo) (eventInfo common.EventInfo, err error) {

	var i os.FileInfo
	var empty common.EventInfo
	eventPath := strings.TrimPrefix(CanonicalPath(event.Path()), c.RootPath)
	normalizedPath := normalize(eventPath)
	if isEventType(EventTypeCreate, event.Event()) || isEventType(EventTypeWrite, event.Event()) {

		var e error
		i, e = c.FS.Stat(eventPath)
		if e != nil {
			if os.IsNotExist(e) {
				log.Println("Ignoring Create/Write event on non-existing file")
				return empty, nil
			}
			return empty, e
		}
		return common.EventInfo{
			Time:           now(),
			Size:           i.Size(),
			Folder:         i.IsDir(),
			Path:           normalizedPath,
			Type:           common.EventCreate,
			PathSyncSource: c,
		}, nil

	} else if isEventType(EventTypeRename, event.Event()) {

		var e error
		i, e = c.FS.Stat(eventPath)
		if e != nil {
			if os.IsNotExist(e) {
				return common.EventInfo{
					Time:           now(),
					Path:           normalizedPath,
					Type:           common.EventRemove,
					PathSyncSource: c,
				}, nil
			}
			return empty, e
		}
		return common.EventInfo{
			Time:           now(),
			Size:           i.Size(),
			Folder:         i.IsDir(),
			Path:           normalizedPath,
			Type:           common.EventRename,
			PathSyncSource: c,
		}, nil

	} else if isEventType(EventTypeDelete, event.Event()) {

		return common.EventInfo{
			Time:           now(),
			Path:           normalizedPath,
			Type:           common.EventRemove,
			PathSyncSource: c,
		}, nil
	} else {
		// Ignore other events
		return empty, nil
	}

}

// FSClient implementation of an endpoint
// Implements all Sync interfaces (PathSyncTarget, PathSyncSource, DataSyncTarget and DataSyncSource)
// Takes a root folder as main parameter
// Underlying calls to FS are done through Afero.FS virtualization, allowing for mockups and automated testings.
type FSClient struct {
	RootPath string
	FS       afero.Fs
}

func (c *FSClient) GetEndpointInfo() common.EndpointInfo {

	return common.EndpointInfo{
		RequiresFoldersRescan: true,
		RequiresNormalization: runtime.GOOS == "darwin",
	}

}

func (c *FSClient) Walk(walknFc common.WalkNodesFunc, pathes ...string) (err error) {
	wrappingFunc := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walknFc("", nil, err)
			return nil
		}
		if len(path) == 0 || path == "/" {
			return nil
		}
		path = normalize(path)
		node, lErr := c.LoadNode(path, !info.IsDir())
		//log.Printf("Walking node %v, %+q => %v, %v", path, path, node, lErr)
		if lErr != nil {
			walknFc("", nil, lErr)
			return nil
		}

		node.MTime = info.ModTime().Unix()
		node.Size = info.Size()
		node.Mode = int32(info.Mode())

		walknFc(path, node, nil)
		return nil
	}
	// Todo : check pathes exists and return error if not ?
	if len(pathes) > 0 {
		for _, path := range pathes {
			// Go be send in concurrency?

			err = afero.Walk(c.FS, denormalize(path), wrappingFunc)
			if err != nil {
				return err
			}
		}
		return nil
	} else {
		return afero.Walk(c.FS, common.InternalPathSeparator, wrappingFunc)
	}
}

// LoadNode is the Read in CRUD.
// leaf bools are used to avoid doing an FS.stat if we already know a node to be
// a leaf.  NOTE : is it useful?  Examine later.
func (c *FSClient) LoadNode(path string, leaf ...bool) (node *tree.Node, err error) {

	denormalizedPath := denormalize(path)
	var isLeaf bool
	var stat os.FileInfo
	var eStat error
	if len(leaf) > 0 {
		isLeaf = leaf[0]
	} else {
		stat, eStat = c.FS.Stat(denormalizedPath)
		if eStat != nil {
			log.Println(eStat)
			return nil, eStat
		}
		isLeaf = !stat.IsDir()
	}
	var hash, uid string
	hash, err = c.getNodeIdentifier(denormalizedPath, isLeaf)
	if err != nil {
		return nil, err
	}
	nodeType := tree.NodeType_LEAF
	if !isLeaf {
		uid = hash
		hash = ""
		nodeType = tree.NodeType_COLLECTION
	}
	node = &tree.Node{
		Path: path,
		Type: nodeType,
		Uuid: uid,
		Etag: hash,
	}
	if stat != nil {
		node.MTime = stat.ModTime().Unix()
		node.Size = stat.Size()
		node.Mode = int32(stat.Mode())
	}
	return node, nil
}

func (c *FSClient) CreateNode(node *tree.Node, updateIfExists bool) (err error) {
	if node.IsLeaf() {
		return errors.New("This is a DataSyncTarget, use PutNode for leafs instead of CreateNode")
	}
	fPath := denormalize(node.Path)
	_, e := c.FS.Stat(fPath)
	if os.IsNotExist(e) {
		err = c.FS.MkdirAll(fPath, 0777)
		if node.Uuid != "" {
			afero.WriteFile(c.FS, filepath.Join(fPath, ".__pydio"), []byte(node.Uuid), 0777)
		}
	}
	return err
}

func (c *FSClient) UpdateNode(node *tree.Node) (err error) {
	return c.CreateNode(node, true)
}

func (c *FSClient) DeleteNode(path string) (err error) {
	_, e := c.FS.Stat(denormalize(path))
	if !os.IsNotExist(e) {
		err = c.FS.RemoveAll(denormalize(path))
	}
	return err
}

// Move file or folder around.
func (c *FSClient) MoveNode(oldPath string, newPath string) (err error) {

	oldPath = denormalize(oldPath)
	newPath = denormalize(newPath)

	stat, e := c.FS.Stat(oldPath)
	if !os.IsNotExist(e) {
		if stat.IsDir() && reflect.TypeOf(c.FS) == reflect.TypeOf(afero.NewMemMapFs()) {
			log.Println("Move Recursively?")
			c.moveRecursively(oldPath, newPath)
		} else {
			err = c.FS.Rename(oldPath, newPath)
		}
	}
	return err

}

// Internal function expects already denormalized form
func (c *FSClient) moveRecursively(oldPath string, newPath string) (err error) {

	// Some fs require moving resources recursively
	moves := make(map[int]string)
	indexes := make([]int, 0)
	i := 0
	afero.Walk(c.FS, oldPath, func(wPath string, info os.FileInfo, err error) error {
		//newWPath := newPath + strings.TrimPrefix(wPath, oldPath)
		indexes = append(indexes, i)
		moves[i] = wPath
		i++
		return nil
	})
	total := len(indexes)
	for key := range indexes {
		//c.FS.Rename(moveK, moveV)
		key = total - key
		wPath := moves[key]
		if len(wPath) == 0 {
			continue
		}
		log.Printf("Moving %v to %v", wPath, newPath+strings.TrimPrefix(wPath, oldPath))
		c.FS.Rename(wPath, newPath+strings.TrimPrefix(wPath, oldPath))
	}
	c.FS.Rename(oldPath, newPath)
	//rename(oldPath,)
	return nil

}

func (c *FSClient) GetWriterOn(path string) (out io.WriteCloser, err error) {

	path = denormalize(path)
	_, e := c.FS.Stat(path)
	var file afero.File
	var openErr error
	if os.IsNotExist(e) {
		file, openErr = c.FS.Create(path)
	} else {
		file, openErr = c.FS.Open(path)
	}
	return file, openErr

}

func (c *FSClient) GetReaderOn(path string) (out io.ReadCloser, err error) {

	return c.FS.Open(denormalize(path))

}

// Expects already denormalized form
func (c *FSClient) getNodeIdentifier(path string, leaf bool) (uid string, e error) {
	if leaf {
		return c.getFileHash(path)
	} else {
		return c.readOrCreateFolderId(path)
	}
}

// Expects already denormalized form
func (c *FSClient) readOrCreateFolderId(path string) (uid string, e error) {

	uidFile, uidErr := c.FS.OpenFile(filepath.Join(path, ".__pydio"), os.O_RDONLY, 0777)
	if uidErr != nil && os.IsNotExist(uidErr) {
		uid = fmt.Sprintf("%s", uuid.NewV4())
		we := afero.WriteFile(c.FS, filepath.Join(path, ".__pydio"), []byte(uid), 0777)
		if we != nil {
			return "", we
		}
	} else {
		uidFile.Close()
		content, re := afero.ReadFile(c.FS, filepath.Join(path, ".__pydio"))
		if re != nil {
			return "", re
		}
		uid = fmt.Sprintf("%s", content)
	}
	return uid, nil

}

// Expects already denormalized form
func (c *FSClient) getFileHash(path string) (hash string, e error) {

	f, err := c.FS.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil

}

// Watch for all fs events on an input path.
// If recursivePath is a non-empty string, it will be concatenated to the
// root path set in FSClient.
func (c *FSClient) Watch(recursivePath string) (*common.WatchObject, error) {

	eventChan := make(chan common.EventInfo)
	errorChan := make(chan error)
	doneChan := make(chan bool)
	// Make the channel buffered to ensure no event is dropped. Notify will drop
	// an event if the receiver is not able to keep up the sending pace.
	in, out := PipeChan(1000)

	var fsEvents []notify.Event
	fsEvents = append(fsEvents, EventTypeAll...)

	recursivePath = denormalize(recursivePath)
	// Check if FS is in-memory
	_, ok := (c.FS).(*afero.MemMapFs)
	if ok {
		log.Println("This is an in-memory FS for testing, return empty structure")
		return &common.WatchObject{
			EventInfoChan: eventChan,
			ErrorChan:     errorChan,
			DoneChan:      doneChan,
		}, nil
	}

	watchPath := filepath.Join(c.RootPath, recursivePath) + "..."
	if e := notify.Watch(watchPath, in, fsEvents...); e != nil {
		return nil, e
	}

	// wait for doneChan to close the watcher, eventChan and errorChan
	go func() {
		<-doneChan

		notify.Stop(in)
		log.Println("Closing event channel for " + c.RootPath)
		close(eventChan)
		close(errorChan)
	}()

	// Get fsnotify notifications for events and errors, and sent them
	// using eventChan and errorChan
	go func() {
		for event := range out {

			if common.IsIgnoredFile(event.Path()) {
				continue
			}

			eventInfo, eventError := notifyEventToEventInfo(c, event)
			if eventError != nil {
				log.Println("Sending  event error for " + c.RootPath)
				errorChan <- eventError
			} else if eventInfo != (common.EventInfo{}) {
				//log.Println("Sending  event info for " + c.RootPath)
				eventChan <- eventInfo
			}

		}
	}()

	return &common.WatchObject{
		EventInfoChan: eventChan,
		ErrorChan:     errorChan,
		DoneChan:      doneChan,
	}, nil
}

// New filesystem endpoint
func New(u *url.URL) *FSClient {
	p := CanonicalPath(u.Path)
	fs := afero.NewBasePathFs(afero.NewOsFs(), p)

	// If the underlying FS is unreachable we have an environment error rather
	// than an application error.
	if _, err := fs.Stat("/"); err != nil {
		// Abandon all hope, ye who enter here...
		panic(errors.Errorf("unreachable block storage %s", p))
	}

	return &FSClient{
		RootPath: p,
		FS:       fs,
	}
}
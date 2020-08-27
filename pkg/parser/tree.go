package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/armon/go-radix"
)

// TreeMap represents a parsed nginx config(s)
// It is the parent struct for parsed data
type TreeMap struct {
	Payload *Payload
	tree    *radix.Tree
	mu      sync.Mutex
}

// NewTree creates a map wrapper around the payload
func NewTree(p *Payload) *TreeMap {
	tm := &TreeMap{Payload: p}
	tm.buildTree()
	return tm
}

// Inserter is the radix tree insert function type
type Inserter func(s string, v interface{}) (interface{}, bool)

// AType is our action type
type AType int

const (
	// ActionUnknown is an uninitialized Action
	ActionUnknown AType = iota

	// ActionInsert insert Directive(s)
	ActionInsert

	// ActionAppend append to Directive
	ActionAppend

	// ActionUpdate update directive
	ActionUpdate

	// ActionDelete deletes an action
	ActionDelete
)

// Changes may be ignored or deleted, I think.
// TODO: fish or cut bait with this
// NOTE: this would be a core of the "change list" to submit a group
//       of config changes as a transaction
type Changes struct {
	Act        AType
	Path       string
	Directives []*Directive
}

const pathSep = "/"

// find a matching directive in the list
// and return its args
// TODO: make this a block method? and the name is not clear
func subVal(name string, blocks []*Directive) []string {
	for _, b := range blocks {
		if b.Directive == name {
			return b.Args
		}
	}
	return nil
}

// WalkBack ties a config path to its associated directives
// NOTE:  payload is implicit, as this will be created and used
//        within a specific payload instance
type WalkBack struct {
	Indicies  []int      // path to that block
	Index     int        // offset into final block slice -- NOTE could make it last entry of Indicies and check for end of slice
	Directive *Directive // TODO: make this a pointer?
}

func (w WalkBack) String() string {
	return fmt.Sprintf("%v-%s", w.Indicies, w.Directive.Directive)
}

// inject blocks into tree
// internal -- named to avoid "insert" for normal use
func (t *TreeMap) inject(Insert Inserter, path string, index []int, blocks []*Directive) {
	debugf("inject path: %s index: %v\n", path, index)
	for i, block := range blocks {
		if block.Directive == "#" {
			continue
		}
		if block.Directive == "include" {
			for _, include := range block.Includes {
				t.inject(Insert, path, []int{include}, t.Payload.Config[include].Parsed)
			}
			continue
		}
		here := path + pathSep + block.Name()
		val := WalkBack{Indicies: index, Directive: block, Index: i}
		Insert(here, val)
		// extend the index and add any children
		sub := append(index, i)
		t.inject(Insert, here, sub, block.Block)
	}
}

// BuildTree does the needful
func (t *TreeMap) buildTree() {
	t.tree = radix.New()
	config := t.Payload.Config[0]
	t.inject(t.tree.Insert, "", []int{0}, config.Parsed)
}

// ShowTree shows the payload config tree
// NOTE: this is effectively a dev tool and may end up being removed
// TODO: What should be exposed via API?
func (t *TreeMap) ShowTree() {
	if len(t.Payload.Config) > 0 {
		fmt.Printf("\nincluded files:\n")
		for i, conf := range t.Payload.Config {
			fmt.Printf("%2d: %s\n", i, conf.File)
		}
		fmt.Println()
	}
	if t.tree == nil {
		t.buildTree()
	}

	// TODO: walk tree to build path/value slices, sort directly
	//       profile to vet such an optimization
	m := t.tree.ToMap()
	paths := make([]string, 0, len(m))
	for k := range m {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	for _, k := range paths {
		fmt.Printf("K: %-60s -- V: %s\n", k, m[k])
	}
}

// Append blocks to the given path
func (t *TreeMap) Append(path string, blocks ...*Directive) error {
	debugf("append path: %s -- %q\n", path, dirs(blocks...))
	wb, err := t.walkback(path)
	if err != nil {
		return err
	}
	var b *[]*Directive
	for i, x := range wb.Indicies {
		if i == 0 {
			b = &(t.Payload.Config[x].Parsed)
			continue
		}
		b = &((*b)[x].Block)
	}
	block := (*b)[wb.Index]
	(*block).Block = append((*block).Block, blocks...)
	// refresh the tree for the new/shifted blocks
	debugf("\n\n APPEND PATH:%s", path)
	t.inject(t.tree.Insert, path, wb.Indicies, block.Block)
	return nil
}

// get path meta info
func (t *TreeMap) walkback(path string) (WalkBack, error) {
	b, ok := t.tree.Get(path)
	if !ok {
		return WalkBack{}, fmt.Errorf("bad path: %q", path)
	}
	wb, ok := b.(WalkBack)
	if !ok {
		return WalkBack{}, fmt.Errorf("wanted %T - got %T", WalkBack{}, wb)
	}
	return wb, nil
}

func dirs(dd ...*Directive) string {
	s := make([]string, len(dd))
	for i, d := range dd {
		s[i] = d.Directive
	}
	return strings.Join(s, ", ")
}

// Insert before the directive indicated by the path
func (t *TreeMap) Insert(path string, inserts ...*Directive) error {
	debugf("insert path: %s -- %q\n", path, dirs(inserts...))
	wb, err := t.walkback(path)
	if err != nil {
		return err
	}
	debugf("HELLO inserts: %v", inserts)

	var block *Directive
	blocks := &(t.Payload.Config[0].Parsed)
	idx := wb.Indicies
	for i, x := range idx {
		switch i {
		case 0:
			// first round is the config in question
			blocks = &(t.Payload.Config[x].Parsed)
		case 1:
			block = ((*blocks)[x])
		default:
			block = ((*block).Block[x])
		}
	}
	if err := block.Insert(wb.Index, inserts...); err != nil {
		return fmt.Errorf("path: %s index: %d err:%w", path, wb.Index, err)
	}
	t.inject(t.tree.Insert, path, idx, block.Block)
	return nil
}

// Delete removes the directive (and children, if any) at the given path
func (t *TreeMap) Delete(path string) error {
	debugf("deleting path: %s\n", path)
	wb, err := t.walkback(path)
	if err != nil {
		return err
	}
	kill := wb.Directive
	var b *[]*Directive
	for i, x := range wb.Indicies {
		if i == 0 {
			b = &(t.Payload.Config[x].Parsed)
			continue
		}
		b = &((*b)[x].Block)
	}
	debugf("deleting from %v\n", b)
	for i, b2 := range *b {
		if b2.Equal(kill) {
			debugf("KILLING: %v\n", kill)
			*b = append((*b)[:i], (*b)[i+1:]...)
			t.tree.Delete(path)
			return nil
		}
	}
	return errors.New("could not delete from path")

}

// ChangeSet allows a transaction of multiple configuration changes
// TODO: ensure it is truly atomic (any failure results in rollback)
func (t *TreeMap) ChangeSet(changes ...Changes) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, change := range changes {
		debugf("CHANGING %d/%d: %q (%d) -> %q\n", i+1, len(changes), change.Path, change.Act, dirs(change.Directives...))
		switch change.Act {
		case ActionInsert:
			if err := t.Insert(change.Path, change.Directives...); err != nil {
				return err
			}
		case ActionAppend:
			if err := t.Append(change.Path, change.Directives...); err != nil {
				return err
			}
		case ActionDelete:
			if err := t.Delete(change.Path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("action #%d -- %+v is not supported at this time", i, change.Act)
		}
	}
	if Debugging {
		t.ShowTree()
	}
	return nil
}

// ChangesLoad unmarshals a json collection of changes
func ChangesLoad(r io.Reader) ([]Changes, error) {
	var c []Changes
	return c, json.NewDecoder(r).Decode(&c)
}

// ChangesFromFile unmarshals a json collection of changes from the given file
func ChangesFromFile(filename string) ([]Changes, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ChangesLoad(f)
}

// WalkFunc represents a radix walk func
type WalkFunc func(string, interface{}) bool

// Matcher evaluates a Directive and returns true if it matches conditions
type Matcher func(*Directive) bool

// Apply is a general purpose function to modify the tree
func (t *TreeMap) Apply(path string, m Matcher, blocks ...*Directive) error {
	wlk := func(path string, obj interface{}) bool {
		debugf("APPLY WALK PATH: %s\n", path)
		wb := obj.(WalkBack)
		if m(wb.Directive) {
			debugf("MATCH APPLYING: %v\n", blocks)
			wb.Directive.Block = append(wb.Directive.Block, blocks...)
			return true
		}
		return false
	}
	t.tree.WalkPrefix(path, wlk)

	return nil
}

// Get returns the value by the give path
func (t *TreeMap) Get(s string) (interface{}, error) {
	if t.tree == nil {
		t.buildTree()
	}
	v, ok := t.tree.Get(s)
	if !ok {
		return nil, fmt.Errorf("entry not found: %q", s)
	}
	wb, ok := v.(WalkBack)
	if !ok {
		return nil, fmt.Errorf("not a WalkBack: %T", v)
	}
	b := wb.Directive
	if len(b.Args) > 0 {
		return strings.Join(b.Args, "-"), nil
	}
	return b, nil
}

// ChangeMe modifies a config with a changeset
func ChangeMe(conf, edit string) (*TreeMap, error) {
	debugf("MODIFY: %s\n", conf)
	debugf("USING : %s\n", edit)

	var catchErrors, single, comment bool
	var ignore []string
	p, err := ParseFile(conf, ignore, catchErrors, single, comment)
	if err != nil {
		log.Printf("Whelp, parsing file %s: %v", conf, err)
		return nil, err
	}

	f, err := os.Open(edit)
	if err != nil {
		return nil, fmt.Errorf("can't open file: %q -- %w", edit, err)
	}
	defer f.Close()

	var changes []Changes
	if err = json.NewDecoder(f).Decode(&changes); err != nil {
		return nil, fmt.Errorf("json decode fail: %w", err)
	}
	tm := &TreeMap{Payload: p}
	tm.buildTree()

	if err = tm.ChangeSet(changes...); err != nil {
		return nil, err
	}

	return tm, tm.Payload.Render(os.Stdout)
}

package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/rpc"
	"path/filepath"
	"strings"

	"github.com/ipfs/go-datastore"
	"github.com/qri-io/cafs"
	ipfs "github.com/qri-io/cafs/ipfs"
	"github.com/qri-io/cafs/memfs"
	"github.com/qri-io/dataset"
	"github.com/qri-io/dataset/detect"
	"github.com/qri-io/dataset/dsfs"
	"github.com/qri-io/dataset/dsio"
	"github.com/qri-io/dataset/validate"
	"github.com/qri-io/dataset/vals"
	"github.com/qri-io/datasetDiffer"
	"github.com/qri-io/jsonschema"
	"github.com/qri-io/qri/p2p"
	"github.com/qri-io/qri/repo"
	"github.com/qri-io/varName"
	diff "github.com/yudai/gojsondiff"
)

// DatasetRequests encapsulates business logic for this node's
// user profile
type DatasetRequests struct {
	repo repo.Repo
	cli  *rpc.Client
	Node *p2p.QriNode
}

// Repo exposes the DatasetRequest's repo
// TODO - this is an architectural flaw resulting from not having a clear
// order of local > network > RPC requests figured out
func (r *DatasetRequests) Repo() repo.Repo {
	return r.repo
}

// CoreRequestsName implements the Requets interface
func (DatasetRequests) CoreRequestsName() string { return "datasets" }

// NewDatasetRequests creates a DatasetRequests pointer from either a repo
// or an rpc.Client
func NewDatasetRequests(r repo.Repo, cli *rpc.Client) *DatasetRequests {
	if r != nil && cli != nil {
		panic(fmt.Errorf("both repo and client supplied to NewDatasetRequests"))
	}

	return &DatasetRequests{
		repo: r,
		cli:  cli,
	}
}

// List returns this repo's datasets
func (r *DatasetRequests) List(p *ListParams, res *[]*repo.DatasetRef) error {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.List", p, res)
	}

	if p.Peername != "" && r.Node != nil {
		replies, err := r.Node.RequestDatasetsList(p.Peername)
		*res = replies
		return err
	} else if p.Peername != "" && r.Node == nil {
		return fmt.Errorf("cannot list datasets of peer without p2p connection")
	}

	store := r.repo.Store()
	// ensure valid limit value
	if p.Limit <= 0 {
		p.Limit = 25
	}
	// ensure valid offset value
	if p.Offset < 0 {
		p.Offset = 0
	}
	replies, err := r.repo.Namespace(p.Limit, p.Offset)
	if err != nil {
		return fmt.Errorf("error getting namespace: %s", err.Error())
	}

	for i, ref := range replies {
		if i >= p.Limit {
			break
		}

		ds, err := dsfs.LoadDataset(store, datastore.NewKey(ref.Path))
		if err != nil {
			// try one extra time...
			// TODO - remove this horrible hack
			ds, err = dsfs.LoadDataset(store, datastore.NewKey(ref.Path))
			if err != nil {
				return fmt.Errorf("error loading path: %s, err: %s", ref.Path, err.Error())
			}
		}
		replies[i].Dataset = ds
	}

	*res = replies
	return nil
}

// Get a dataset
func (r *DatasetRequests) Get(p *repo.DatasetRef, res *repo.DatasetRef) error {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Get", p, res)
	}
	getRemote := func(err error) error {
		if r.Node != nil {
			ref, err := r.Node.RequestDatasetInfo(p)
			if ref != nil {
				ds := ref.Dataset
				// TODO - this is really stupid, p2p.RequestDatasetInfo should return an error here
				if ds.IsEmpty() {
					return fmt.Errorf("dataset not found")
				}
				st := ds.Structure
				// TODO - it seems that jsonschema.RootSchema and encoding/gob
				// really don't like each other (no surprise there, thanks to validators being an interface)
				// Which means that when this response is proxied over the wire bad things happen
				// So for now we're doing this weirdness with re-creating a gob-friendly version
				// of a dataset
				*res = repo.DatasetRef{
					Peername: p.Peername,
					Name:     p.Name,
					Path:     ref.Path,
					Dataset: &dataset.Dataset{
						Commit: ds.Commit,
						Meta:   ds.Meta,
						Structure: &dataset.Structure{
							Checksum:     st.Checksum,
							Compression:  st.Compression,
							Encoding:     st.Encoding,
							ErrCount:     st.ErrCount,
							Entries:      st.Entries,
							Format:       st.Format,
							FormatConfig: st.FormatConfig,
							Length:       st.Length,
							Qri:          st.Qri,
						},
					},
				}
			}
			return err
		}
		return err
	}

	store := r.repo.Store()

	if p.Path == "" {
		path, err := r.repo.GetPath(p.Name)
		if err != nil {
			err = fmt.Errorf("error loading path for name: %s", err.Error())
			return getRemote(err)
		}
		p.Path = path.String()
	} else if !strings.HasSuffix(p.Path, "/"+dsfs.PackageFileDataset.String()) && strings.HasPrefix(p.Path, "/ipfs/") {
		// TODO - this is silly. need the "hasPrefix" to make tests pass (tests use a map store)
		p.Path = fmt.Sprintf("%s/%s", p.Path, dsfs.PackageFileDataset.String())
	}
	ds, err := dsfs.LoadDataset(store, datastore.NewKey(p.Path))
	if err != nil {
		return getRemote(err)
	}

	name := p.Name
	if p.Path != "" {
		name, _ = r.repo.GetName(datastore.NewKey(p.Path))
	}

	*res = repo.DatasetRef{
		Peername: p.Peername,
		Name:     name,
		Path:     p.Path,
		Dataset:  ds,
	}
	return nil
}

// InitParams encapsulates arguments to Init
type InitParams struct {
	Name             string    // variable name for referring to this dataset. required.
	URL              string    // url to download data from. either Url or Data is required
	DataFilename     string    // filename of data file. extension is used for filetype detection
	Data             io.Reader // reader of structured data. either Url or Data is required
	MetadataFilename string    // filename of metadata file. optional.
	Metadata         io.Reader // reader of json-formatted metadata
	// TODO
	// StructureFilename string    // filename of metadata file. optional.
	// Structure         io.Reader // reader of json-formatted metadata
	// TODO - add support for adding via path/hash
	// DataPath         datastore.Key // path to structured data
}

// Init creates a new qri dataset from a source of data
func (r *DatasetRequests) Init(p *InitParams, res *repo.DatasetRef) error {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Init", p, res)
	}

	var (
		rdr      io.Reader
		store    = r.repo.Store()
		filename = p.DataFilename
	)

	if p.URL != "" {
		res, err := http.Get(p.URL)
		if err != nil {
			return fmt.Errorf("error fetching url: %s", err.Error())
		}
		filename = filepath.Base(p.URL)
		defer res.Body.Close()
		rdr = res.Body
	} else if p.Data != nil {
		rdr = p.Data
	} else {
		return fmt.Errorf("either a file or a url is required to create a dataset")
	}

	if p.Name != "" {
		if err := validate.ValidName(p.Name); err != nil {
			return fmt.Errorf("invalid name: %s", err.Error())
		}
	}

	// TODO - need a better strategy for huge files
	data, err := ioutil.ReadAll(rdr)
	if err != nil {
		return fmt.Errorf("error reading file: %s", err.Error())
	}

	st, err := detect.FromReader(filename, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("error determining dataset schema: %s", err.Error())
	}
	// Ensure that dataset contains valid field names
	if err = validate.Structure(st); err != nil {
		return fmt.Errorf("invalid structure: %s", err.Error())
	}

	// TODO - restore
	// if err := validate.DataFormat(st.Format, bytes.NewReader(data)); err != nil {
	// 	return fmt.Errorf("invalid data format: %s", err.Error())
	// }

	// TODO - check for errors in dataset and warn user if errors exist

	datakey, err := store.Put(memfs.NewMemfileBytes("data."+st.Format.String(), data), false)
	if err != nil {
		return fmt.Errorf("error putting data file in store: %s", err.Error())
	}

	dataexists, err := repo.HasPath(r.repo, datakey)
	if err != nil && !strings.Contains(err.Error(), repo.ErrRepoEmpty.Error()) {
		return fmt.Errorf("error checking repo for already-existing data: %s", err.Error())
	}
	if dataexists {
		return fmt.Errorf("this data already exists")
	}

	name := p.Name
	if name == "" && filename != "" {
		name = varName.CreateVarNameFromString(filename)
	}

	ds := &dataset.Dataset{
		Meta:      &dataset.Meta{},
		Commit:    &dataset.Commit{Title: "initial commit"},
		Structure: st,
	}
	if p.URL != "" {
		ds.Meta.DownloadPath = p.URL
		// if we're adding from a dataset url, set a default accrual periodicity of once a week
		// this'll set us up to re-check urls over time
		// TODO - make this configurable via a param?
		ds.Meta.AccrualPeriodicity = "R/P1W"
	}
	if p.Metadata != nil {
		if ds.Meta == nil {
			ds.Meta = &dataset.Meta{}
		}
		if err := json.NewDecoder(p.Metadata).Decode(ds.Meta); err != nil {
			return fmt.Errorf("error parsing metadata json: %s", err.Error())
		}
	}

	dataf := memfs.NewMemfileBytes("data."+st.Format.String(), data)
	dskey, err := r.repo.CreateDataset(ds, dataf, true)
	if err != nil {
		fmt.Printf("error creating dataset: %s\n", err.Error())
		return err
	}

	if err = r.repo.PutName(name, dskey); err != nil {
		return fmt.Errorf("error adding dataset name to repo: %s", err.Error())
	}

	ds, err = r.repo.GetDataset(dskey)
	if err != nil {
		return fmt.Errorf("error reading dataset: '%s': %s", dskey.String(), err.Error())
	}

	*res = repo.DatasetRef{
		Name:    p.Name,
		Path:    dskey.String(),
		Dataset: ds,
	}
	return nil
}

// SaveParams defines permeters for Dataset Saves
type SaveParams struct {
	Prev         *repo.DatasetRef
	Changes      *dataset.Dataset // all dataset changes. required.
	DataFilename string           // filename for new data. optional.
	Data         io.Reader        // stream of complete dataset update. optional.
}

// Save adds a history entry, updating a dataset
// TODO - need to make sure users aren't forking by referncing commits other than tip
func (r *DatasetRequests) Save(p *SaveParams, res *repo.DatasetRef) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Save", p, res)
	}

	var (
		ds          = &dataset.Dataset{}
		dataf       cafs.File
		commitTitle string
	)

	prev := &repo.DatasetRef{}
	fmt.Println(p.Prev)
	if err := r.Get(p.Prev, prev); err != nil {
		return fmt.Errorf("error getting previous dataset: %s", err.Error())
	}

	if p.Changes.Commit != nil {
		commitTitle = p.Changes.Commit.Title
	}

	// add all previous fields and any changes
	ds.Assign(prev.Dataset, p.Changes)
	ds.PreviousPath = prev.Path

	// ds.Assign clobbers empty commit messages with the previous
	// commit message. So if the peer hasn't provided a message at this point
	// let's maintain that going into CreateDataset
	if commitTitle == "" {
		p.Changes.Commit.Title = ""
	}

	if p.Data != nil {
		dataf = memfs.NewMemfileReader(p.DataFilename, p.Data)
	}

	dspath, err := r.repo.CreateDataset(ds, dataf, true)
	if err != nil {
		fmt.Println("create ds error: %s", err.Error())
		return err
	}

	fmt.Println(ds.PreviousPath)

	if p.Prev.Name != "" {
		if err := r.repo.DeleteName(p.Prev.Name); err != nil {
			return err
		}
		if err := r.repo.PutName(p.Prev.Name, dspath); err != nil {
			return err
		}
	}

	*res = repo.DatasetRef{
		Peername: p.Prev.Peername,
		Name:     p.Prev.Name,
		Path:     dspath.String(),
		Dataset:  ds,
	}

	return nil
}

// RenameParams defines parameters for Dataset renaming
type RenameParams struct {
	Current, New string
}

// Rename changes a user's given name for a dataset
func (r *DatasetRequests) Rename(p *RenameParams, res *repo.DatasetRef) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Rename", p, res)
	}

	if p.Current == "" {
		return fmt.Errorf("current name is required to rename a dataset")
	}

	if err := validate.ValidName(p.New); err != nil {
		return err
	}

	if _, err := r.repo.GetPath(p.New); err != repo.ErrNotFound {
		return fmt.Errorf("name '%s' already exists", p.New)
	}

	path, err := r.repo.GetPath(p.Current)
	if err != nil {
		return fmt.Errorf("error getting dataset: %s", err.Error())
	}
	if err := r.repo.DeleteName(p.Current); err != nil {
		return err
	}
	if err := r.repo.PutName(p.New, path); err != nil {
		return err
	}

	ds, err := dsfs.LoadDataset(r.repo.Store(), path)
	if err != nil {
		return err
	}

	*res = repo.DatasetRef{
		Name:    p.New,
		Path:    path.String(),
		Dataset: ds,
	}
	return nil
}

// Remove a dataset
func (r *DatasetRequests) Remove(p *repo.DatasetRef, ok *bool) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Remove", p, ok)
	}

	if p.Path == "" && p.Name == "" {
		return fmt.Errorf("either name or path is required")
	}

	if p.Path == "" {
		key, e := r.repo.GetPath(p.Name)
		if e != nil {
			return e
		}
		p.Path = key.String()
	}

	p.Name, err = r.repo.GetName(datastore.NewKey(p.Path))
	if err != nil {
		return
	}

	if pinner, ok := r.repo.Store().(cafs.Pinner); ok {
		path := datastore.NewKey(strings.TrimSuffix(p.Path, "/"+dsfs.PackageFileDataset.String()))
		if err = pinner.Unpin(path, true); err != nil {
			return
		}
	}

	if err = r.repo.DeleteName(p.Name); err != nil {
		return
	}

	*ok = true
	return nil
}

// StructuredDataParams defines parameters for retrieving
// structured data (which is the kind of data datasets contain)
type StructuredDataParams struct {
	Format        dataset.DataFormat
	FormatConfig  dataset.FormatConfig
	Path          datastore.Key
	Limit, Offset int
	All           bool
}

// StructuredData combines data with it's hashed path
type StructuredData struct {
	Path datastore.Key `json:"path"`
	Data interface{}   `json:"data"`
}

// StructuredData retrieves dataset data
func (r *DatasetRequests) StructuredData(p *StructuredDataParams, data *StructuredData) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.StructuredData", p, data)
	}

	var (
		file  cafs.File
		d     []byte
		store = r.repo.Store()
	)

	if p.Limit < 0 || p.Offset < 0 {
		return fmt.Errorf("invalid limit / offset settings")
	}

	ds, err := dsfs.LoadDataset(store, p.Path)
	if err != nil {
		return err
	}

	if p.All {
		file, err = dsfs.LoadData(store, ds)
	} else {
		d, err = dsfs.LoadRows(store, ds, p.Limit, p.Offset)
		file = memfs.NewMemfileBytes("data", d)
	}

	if err != nil {
		return err
	}

	st := &dataset.Structure{}
	st.Assign(ds.Structure, &dataset.Structure{
		Format:       p.Format,
		FormatConfig: p.FormatConfig,
	})

	buf, err := dsio.NewValueBuffer(st)
	if err != nil {
		return fmt.Errorf("error allocating result buffer: %s", err)
	}
	rr, err := dsio.NewValueReader(ds.Structure, file)
	if err != nil {
		return fmt.Errorf("error allocating data reader: %s", err)
	}
	if err = dsio.EachValue(rr, func(i int, val vals.Value, err error) error {
		if err != nil {
			return err
		}
		return buf.WriteValue(val)
	}); err != nil {
		return fmt.Errorf("row iteration error: %s", err.Error())
	}

	if err := buf.Close(); err != nil {
		return fmt.Errorf("error closing row buffer: %s", err.Error())
	}

	*data = StructuredData{
		Path: p.Path,
		Data: json.RawMessage(buf.Bytes()),
	}
	return nil
}

// Add adds an existing dataset to a peer's repository
func (r *DatasetRequests) Add(ref *repo.DatasetRef, res *repo.DatasetRef) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Add", ref, res)
	}

	if ref.Path == "" && r.Node != nil {
		res, err := r.Node.RequestDatasetInfo(ref)
		if err != nil {
			return err
		}
		fmt.Println(res)
		ref = res
		fmt.Println(ref.Path)
	}

	fs, ok := r.repo.Store().(*ipfs.Filestore)
	if !ok {
		return fmt.Errorf("can only add datasets when running an IPFS filestore")
	}

	key := datastore.NewKey(strings.TrimSuffix(ref.Path, "/"+dsfs.PackageFileDataset.String()))
	_, err = fs.Fetch(cafs.SourceAny, key)
	if err != nil {
		return fmt.Errorf("error fetching file: %s", err.Error())
	}

	err = fs.Pin(key, true)
	if err != nil {
		return fmt.Errorf("error pinning root key: %s", err.Error())
	}

	path := datastore.NewKey(key.String() + "/" + dsfs.PackageFileDataset.String())
	err = r.repo.PutName(ref.Name, path)
	if err != nil {
		return fmt.Errorf("error putting dataset name in repo: %s", err.Error())
	}

	ds, err := dsfs.LoadDataset(fs, path)
	if err != nil {
		return fmt.Errorf("error loading newly saved dataset path: %s", path.String())
	}

	*res = repo.DatasetRef{
		Name:    ref.Name,
		Path:    path.String(),
		Dataset: ds,
	}
	return
}

// ValidateDatasetParams defines paremeters for dataset
// data validation
type ValidateDatasetParams struct {
	Name string
	// URL          string
	Path         datastore.Key
	DataFilename string
	Data         io.Reader
	Schema       io.Reader
}

// Validate gives a dataset of errors and issues for a given dataset
func (r *DatasetRequests) Validate(p *ValidateDatasetParams, errors *[]jsonschema.ValError) (err error) {
	if r.cli != nil {
		return r.cli.Call("DatasetRequests.Validate", p, errors)
	}

	var (
		sch  *jsonschema.RootSchema
		ref  *repo.DatasetRef
		data []byte
	)

	// if a dataset is specified, load it
	if p.Name != "" || p.Path.String() != "" {
		ref = &repo.DatasetRef{}
		err = r.Get(&repo.DatasetRef{
			Name: p.Name,
			Path: p.Path.String(),
		}, ref)

		if err != nil {
			return err
		}
		sch = ref.Dataset.Structure.Schema
	}

	// if a schema is specified, override with it
	if p.Schema != nil {
		stbytes, err := ioutil.ReadAll(p.Schema)
		if err != nil {
			return err
		}
		sch = &jsonschema.RootSchema{}
		if e := sch.UnmarshalJSON(stbytes); err != nil {
			return e
		}
	}

	if p.Data != nil {
		data, err = ioutil.ReadAll(p.Data)
		if err != nil {
			return
		}

		// if no schema, detect one
		if sch == nil {
			st, e := detect.FromReader(p.DataFilename, p.Data)
			if e != nil {
				return e
			}
			sch = st.Schema
		}
	}

	if data == nil && ref != nil {
		f, e := dsfs.LoadData(r.repo.Store(), ref.Dataset)
		if e != nil {
			return e
		}

		if ref.Dataset.Structure.Format != dataset.JSONDataFormat {
			// convert to JSON bytes if necessary
			vr, e := dsio.NewValueReader(ref.Dataset.Structure, f)
			if e != nil {
				return e
			}

			buf, err := dsio.NewValueBuffer(&dataset.Structure{
				Format: dataset.JSONDataFormat,
				Schema: ref.Dataset.Structure.Schema,
			})

			err = dsio.EachValue(vr, func(i int, val vals.Value, err error) error {
				if err != nil {
					return err
				}
				return buf.WriteValue(val)
			})

			if err != nil {
				return err
			}
			if e := buf.Close(); err != nil {
				return e
			}
			data = buf.Bytes()
		} else {
			data, err = ioutil.ReadAll(f)
			if err != nil {
				return
			}
		}
	}

	*errors = sch.ValidateBytes(data)
	return
}

// DiffParams defines parameters for diffing two datasets with Diff
type DiffParams struct {
	// The pointers to the datasets to diff
	DsLeft, DsRight *dataset.Dataset
	// override flag to diff full dataset without having to specify each component
	DiffAll bool
	// if DiffAll is false, DiffComponents specifies which components of a dataset to diff
	// currently supported components include "structure", "data", "meta", "transform", and "visConfig"
	DiffComponents map[string]bool
}

// Diff computes the diff of two datasets
func (r *DatasetRequests) Diff(p *DiffParams, diffs *map[string]diff.Diff) (err error) {
	diffMap := map[string]diff.Diff{}
	if p.DiffAll {
		diffMap, err := datasetDiffer.DiffDatasets(p.DsLeft, p.DsRight)
		if err != nil {
			return fmt.Errorf("error diffing datasets: %s", err.Error())
		}
		// TODO: remove this temporary hack
		if diffMap["data"] == nil || len(diffMap["data"].Deltas()) == 0 {
			// dereference data paths
			// marshal json to []byte
			// call `datasetDiffer.DiffJSON(a, b)`
		}
		*diffs = diffMap
	} else {
		for k, v := range p.DiffComponents {
			if v {
				switch k {
				case "structure":
					if p.DsLeft.Structure != nil && p.DsRight.Structure != nil {
						structureDiffs, err := datasetDiffer.DiffStructure(p.DsLeft.Structure, p.DsRight.Structure)
						if err != nil {
							return fmt.Errorf("error diffing structure: %s", err.Error())
						}
						diffMap[k] = structureDiffs
					}
				case "data":
					//TODO
					if p.DsLeft.DataPath != "" && p.DsRight.DataPath != "" {
						dataDiffs, err := datasetDiffer.DiffData(p.DsLeft, p.DsRight)
						if err != nil {
							return fmt.Errorf("error diffing data: %s", err.Error())
						}

						diffMap[k] = dataDiffs
					}
				case "transform":
					if p.DsLeft.Transform != nil && p.DsRight.Transform != nil {
						transformDiffs, err := datasetDiffer.DiffTransform(p.DsLeft.Transform, p.DsRight.Transform)
						if err != nil {
							return fmt.Errorf("error diffing transform: %s", err.Error())
						}
						diffMap[k] = transformDiffs
					}
				case "meta":
					if p.DsLeft.Meta != nil && p.DsRight.Meta != nil {
						metaDiffs, err := datasetDiffer.DiffMeta(p.DsLeft.Meta, p.DsRight.Meta)
						if err != nil {
							return fmt.Errorf("error diffing meta: %s", err.Error())
						}
						diffMap[k] = metaDiffs
					}
				case "visConfig":
					if p.DsLeft.VisConfig != nil && p.DsRight.VisConfig != nil {
						visConfigDiffs, err := datasetDiffer.DiffVisConfig(p.DsLeft.VisConfig, p.DsRight.VisConfig)
						if err != nil {
							return fmt.Errorf("error diffing visConfig: %s", err.Error())
						}
						diffMap[k] = visConfigDiffs
					}
				}
			}
		}
		*diffs = diffMap

	}
	// Hack to examine data
	if p.DiffAll || p.DiffComponents["data"] == true {
		sd1Params := &StructuredDataParams{
			Format:       dataset.JSONDataFormat,
			FormatConfig: &dataset.JSONOptions{ArrayEntries: true},
			Path:         p.DsLeft.Path(),
		}
		sd2Params := &StructuredDataParams{
			Format:       dataset.JSONDataFormat,
			FormatConfig: &dataset.JSONOptions{ArrayEntries: true},
			Path:         p.DsRight.Path(),
		}
		sd1 := &StructuredData{}
		sd2 := &StructuredData{}
		err := r.StructuredData(sd1Params, sd1)
		if err != nil {
			return fmt.Errorf("error getting structured data: %s", err.Error())
		}
		err = r.StructuredData(sd2Params, sd2)
		if err != nil {
			return fmt.Errorf("error getting structured data: %s", err.Error())
		}
		var jsonRaw1, jsonRaw2 json.RawMessage
		var ok bool
		if jsonRaw1, ok = sd1.Data.(json.RawMessage); !ok {
			return fmt.Errorf("error getting json raw message")
		}
		if jsonRaw2, ok = sd2.Data.(json.RawMessage); !ok {
			return fmt.Errorf("error getting json raw message")
		}
		m1 := &map[string]json.RawMessage{"structure": jsonRaw1}
		m2 := &map[string]json.RawMessage{"structure": jsonRaw2}
		dataBytes1, err := json.Marshal(m1)
		if err != nil {
			return fmt.Errorf("error marshaling json: %s", err.Error())
		}
		dataBytes2, err := json.Marshal(m2)
		if err != nil {
			return fmt.Errorf("error marshaling json: %s", err.Error())
		}
		dataDiffs, err := datasetDiffer.DiffJSON(dataBytes1, dataBytes2)
		if err != nil {
			return fmt.Errorf("error comparing structured data: %s", err.Error())
		}
		diffMap["data"] = dataDiffs
	}
	return nil
}

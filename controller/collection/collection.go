package collection

import (
	"errors"
	"encoding/json"

	"github.com/tidwall/btree"
	"github.com/tidwall/tile38/geojson"
	"github.com/tidwall/tile38/index"
)

const (
	idOrdered    = 0
	valueOrdered = 1
)

type itemT struct {
	id     string
	object geojson.Object
	fields []float64
}

func (i *itemT) Less(item btree.Item, ctx interface{}) bool {
	switch ctx {
	default:
		return false
	case idOrdered:
		return i.id < item.(*itemT).id
	case valueOrdered:
		i1, i2 := i.object.String(), item.(*itemT).object.String()
		if i1 < i2 {
			return true
		}
		if i1 > i2 {
			return false
		}
		// the values match so we will compare the ids, which are always unique.
		return i.id < item.(*itemT).id
	}
}

func (i *itemT) Rect() (minX, minY, minZ, maxX, maxY, maxZ float64) {
	bbox := i.object.CalculatedBBox()
	return bbox.Min.X, bbox.Min.Y, bbox.Min.Z, bbox.Max.X, bbox.Max.Y, bbox.Max.Z
}

func (i *itemT) Point() (x, y, z float64) {
	x, y, z, _, _, _ = i.Rect()
	return
}

type Row struct {
	Id     string    `json:"id"`
	Obj    []byte    `json:"obj"`
	Values []float64 `json:"values"`
}

type Portable struct {
	Rows   []Row    `json:"rows"`
	Fields []string `json:"fields"`
}

func (c *Collection) MarshalJSON() ([]byte, error) {

	colCount := c.Count()
	portable := Portable{
		Rows:   make([]Row, colCount),
		Fields: c.FieldArr(),
	}

	var i int
	c.Scan(0, false,
		func(id string, obj geojson.Object, values []float64) bool {
			// Bounds check for safety
			if i < colCount {
				objBytes, _ := obj.MarshalJSON()

				portable.Rows[i] = Row{
					Id:     id,
					Obj:    objBytes,
					Values: values,
				}
				i++
				return true
			}
			return false
		},
	)

	return json.Marshal(portable)
}

func (c *Collection) UnmarshalJSON(b []byte) error {

	if b == nil {
		return errors.New("No bytes were input")
	}

	portable := Portable{}
	portable.Rows = make([]Row, 0)

	if err := json.Unmarshal(b, &portable); err != nil {
		return err
	}

	for i := range portable.Rows {
		obj, err := geojson.ObjectAuto(portable.Rows[i].Obj)
		if err != nil {
			return err
		}
		c.ReplaceOrInsert(portable.Rows[i].Id, obj, portable.Fields, portable.Rows[i].Values)
	}

	return nil
}

// Collection represents a collection of geojson objects.
type Collection struct {
	items    *btree.BTree // items sorted by keys
	values   *btree.BTree // items sorted by value+key
	index    *index.Index // items geospatially indexed
	fieldMap map[string]int
	weight   int
	points   int
	objects  int // geometry count
	nobjects int // non-geometry count
}

var counter uint64

// New creates an empty collection
func New() *Collection {
	col := &Collection{
		index:    index.New(),
		items:    btree.New(16, idOrdered),
		values:   btree.New(16, valueOrdered),
		fieldMap: make(map[string]int),
	}
	return col
}

// Count returns the number of objects in collection.
func (c *Collection) Count() int {
	return c.objects + c.nobjects
}

// PointCount returns the number of points (lat/lon coordinates) in collection.
func (c *Collection) PointCount() int {
	return c.points
}

// TotalWeight calculates the in-memory cost of the collection in bytes.
func (c *Collection) TotalWeight() int {
	return c.weight
}

// Bounds returns the bounds of all the items in the collection.
func (c *Collection) Bounds() (minX, minY, minZ, maxX, maxY, maxZ float64) {
	return c.index.Bounds()
}

// ReplaceOrInsert adds or replaces an object in the collection and returns the fields array.
// If an item with the same id is already in the collection then the new item will adopt the old item's fields.
// The fields argument is optional.
// The return values are the old object, the old fields, and the new fields
func (c *Collection) ReplaceOrInsert(id string, obj geojson.Object, fields []string, values []float64) (oldObject geojson.Object, oldFields []float64, newFields []float64) {
	oldItem, ok := c.remove(id)
	nitem := c.insert(id, obj)
	if ok {
		oldObject = oldItem.object
		oldFields = oldItem.fields
		nitem.fields = oldFields
		c.weight += len(nitem.fields) * 8
	}
	if fields == nil && len(values) > 0 {
		// directly set the field values, update weight
		c.weight -= len(nitem.fields) * 8
		nitem.fields = values
		c.weight += len(nitem.fields) * 8

	} else {
		// map field name to value
		for i, field := range fields {
			c.setField(nitem, field, values[i])
		}
	}
	return oldObject, oldFields, nitem.fields
}

func (c *Collection) remove(id string) (item *itemT, ok bool) {
	i := c.items.Delete(&itemT{id: id})
	if i == nil {
		return nil, false
	}
	item = i.(*itemT)
	if item.object.IsGeometry() {
		c.index.Remove(item)
		c.objects--
	} else {
		c.values.Delete(item)
		c.nobjects--
	}
	c.weight -= len(item.fields) * 8
	c.weight -= item.object.Weight() + len(item.id)
	c.points -= item.object.PositionCount()
	return item, true
}

func (c *Collection) insert(id string, obj geojson.Object) (item *itemT) {
	item = &itemT{id: id, object: obj}
	if obj.IsGeometry() {
		c.index.Insert(item)
		c.objects++
	} else {
		c.values.ReplaceOrInsert(item)
		c.nobjects++
	}
	c.items.ReplaceOrInsert(item)
	c.weight += obj.Weight() + len(id)
	c.points += obj.PositionCount()
	return item
}

// Remove removes an object and returns it.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Remove(id string) (obj geojson.Object, fields []float64, ok bool) {
	item, ok := c.remove(id)
	if !ok {
		return nil, nil, false
	}
	return item.object, item.fields, true
}

func (c *Collection) get(id string) (obj geojson.Object, fields []float64, ok bool) {
	i := c.items.Get(&itemT{id: id})
	if i == nil {
		return nil, nil, false
	}
	item := i.(*itemT)
	return item.object, item.fields, true
}

// Get returns an object.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Get(id string) (obj geojson.Object, fields []float64, ok bool) {
	return c.get(id)
}

// SetField set a field value for an object and returns that object.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) SetField(id, field string, value float64) (obj geojson.Object, fields []float64, updated bool, ok bool) {
	i := c.items.Get(&itemT{id: id})
	if i == nil {
		ok = false
		return
	}
	item := i.(*itemT)
	updated = c.setField(item, field, value)
	return item.object, item.fields, updated, true
}

func (c *Collection) setField(item *itemT, field string, value float64) (updated bool) {
	idx, ok := c.fieldMap[field]
	if !ok {
		idx = len(c.fieldMap)
		c.fieldMap[field] = idx
	}
	c.weight -= len(item.fields) * 8
	for idx >= len(item.fields) {
		item.fields = append(item.fields, 0)
	}
	c.weight += len(item.fields) * 8
	ovalue := item.fields[idx]
	item.fields[idx] = value
	return ovalue != value
}

// FieldMap return a maps of the field names.
func (c *Collection) FieldMap() map[string]int {
	return c.fieldMap
}

// FieldArr return an array representation of the field names.
func (c *Collection) FieldArr() []string {
	arr := make([]string, len(c.fieldMap))
	for field, i := range c.fieldMap {
		arr[i] = field
	}
	return arr
}

// Scan iterates though the collection ids. A cursor can be used for paging.
func (c *Collection) Scan(cursor uint64, desc bool,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) (ncursor uint64) {
	var i uint64
	var active = true
	iter := func(item btree.Item) bool {
		if i >= cursor {
			iitm := item.(*itemT)
			active = iterator(iitm.id, iitm.object, iitm.fields)
		}
		i++
		return active
	}
	if desc {
		c.items.Descend(iter)
	} else {
		c.items.Ascend(iter)
	}
	return i
}

// ScanGreaterOrEqual iterates though the collection starting with specified id. A cursor can be used for paging.
func (c *Collection) ScanRange(cursor uint64, start, end string, desc bool,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) (ncursor uint64) {
	var i uint64
	var active = true
	iter := func(item btree.Item) bool {
		if i >= cursor {
			iitm := item.(*itemT)
			active = iterator(iitm.id, iitm.object, iitm.fields)
		}
		i++
		return active
	}

	if desc {
		c.items.DescendRange(&itemT{id: start}, &itemT{id: end}, iter)
	} else {
		c.items.AscendRange(&itemT{id: start}, &itemT{id: end}, iter)
	}
	return i
}

// SearchValues iterates though the collection values. A cursor can be used for paging.
func (c *Collection) SearchValues(cursor uint64, desc bool,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) (ncursor uint64) {
	var i uint64
	var active = true
	iter := func(item btree.Item) bool {
		if i >= cursor {
			iitm := item.(*itemT)
			active = iterator(iitm.id, iitm.object, iitm.fields)
		}
		i++
		return active
	}
	if desc {
		c.values.Descend(iter)
	} else {
		c.values.Ascend(iter)
	}
	return i
}

// SearchValuesRange iterates though the collection values. A cursor can be used for paging.
func (c *Collection) SearchValuesRange(cursor uint64, start, end string, desc bool,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) (ncursor uint64) {
	var i uint64
	var active = true
	iter := func(item btree.Item) bool {
		if i >= cursor {
			iitm := item.(*itemT)
			active = iterator(iitm.id, iitm.object, iitm.fields)
		}
		i++
		return active
	}
	if desc {
		c.values.DescendRange(&itemT{object: geojson.String(start)}, &itemT{object: geojson.String(end)}, iter)
	} else {
		c.values.AscendRange(&itemT{object: geojson.String(start)}, &itemT{object: geojson.String(end)}, iter)
	}
	return i
}

// ScanGreaterOrEqual iterates though the collection starting with specified id. A cursor can be used for paging.
func (c *Collection) ScanGreaterOrEqual(id string, cursor uint64, desc bool,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) (ncursor uint64) {
	var i uint64
	var active = true
	iter := func(item btree.Item) bool {
		if i >= cursor {
			iitm := item.(*itemT)
			active = iterator(iitm.id, iitm.object, iitm.fields)
		}
		i++
		return active
	}
	if desc {
		c.items.DescendLessOrEqual(&itemT{id: id}, iter)
	} else {
		c.items.AscendGreaterOrEqual(&itemT{id: id}, iter)
	}
	return i
}

func (c *Collection) geoSearch(cursor uint64, bbox geojson.BBox, iterator func(id string, obj geojson.Object, fields []float64) bool) (ncursor uint64) {
	return c.index.Search(cursor, bbox.Min.Y, bbox.Min.X, bbox.Max.Y, bbox.Max.X, bbox.Min.Z, bbox.Max.Z, func(item index.Item) bool {
		var iitm *itemT
		iitm, ok := item.(*itemT)
		if !ok {
			return true // just ignore
		}
		if !iterator(iitm.id, iitm.object, iitm.fields) {
			return false
		}
		return true
	})
}

// Nearby returns all object that are nearby a point.
func (c *Collection) Nearby(cursor uint64, sparse uint8, lat, lon, meters, minZ, maxZ float64, iterator func(id string, obj geojson.Object, fields []float64) bool) (ncursor uint64) {
	center := geojson.Position{X: lon, Y: lat, Z: 0}
	bbox := geojson.BBoxesFromCenter(lat, lon, meters)
	bboxes := bbox.Sparse(sparse)
	if sparse > 0 {
		for _, bbox := range bboxes {
			bbox.Min.Z, bbox.Max.Z = minZ, maxZ
			c.geoSearch(cursor, bbox, func(id string, obj geojson.Object, fields []float64) bool {
				if obj.Nearby(center, meters) {
					if iterator(id, obj, fields) {
						return false
					}
				}
				return true
			})
		}
		return 0
	}
	bbox.Min.Z, bbox.Max.Z = minZ, maxZ
	return c.geoSearch(cursor, bbox, func(id string, obj geojson.Object, fields []float64) bool {
		if obj.Nearby(center, meters) {
			return iterator(id, obj, fields)
		}
		return true
	})
}

// Within returns all object that are fully contained within an object or bounding box. Set obj to nil in order to use the bounding box.
func (c *Collection) Within(cursor uint64, sparse uint8, obj geojson.Object, minLat, minLon, maxLat, maxLon, minZ, maxZ float64, iterator func(id string, obj geojson.Object, fields []float64) bool) (ncursor uint64) {
	var bbox geojson.BBox
	if obj != nil {
		bbox = obj.CalculatedBBox()
	} else {
		bbox = geojson.BBox{Min: geojson.Position{X: minLon, Y: minLat, Z: minZ}, Max: geojson.Position{X: maxLon, Y: maxLat, Z: maxZ}}
	}
	bboxes := bbox.Sparse(sparse)
	if sparse > 0 {
		for _, bbox := range bboxes {
			if obj != nil {
				c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
					if o.Within(obj) {
						if iterator(id, o, fields) {
							return false
						}
					}
					return true
				})
			}
			c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
				if o.WithinBBox(bbox) {
					if iterator(id, o, fields) {
						return false
					}
				}
				return true
			})
		}
		return 0
	}
	if obj != nil {
		return c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
			if o.Within(obj) {
				return iterator(id, o, fields)
			}
			return true
		})
	}
	return c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
		if o.WithinBBox(bbox) {
			return iterator(id, o, fields)
		}
		return true
	})
}

// Intersects returns all object that are intersect an object or bounding box. Set obj to nil in order to use the bounding box.
func (c *Collection) Intersects(cursor uint64, sparse uint8, obj geojson.Object, minLat, minLon, maxLat, maxLon, maxZ, minZ float64, iterator func(id string, obj geojson.Object, fields []float64) bool) (ncursor uint64) {
	var bbox geojson.BBox
	if obj != nil {
		bbox = obj.CalculatedBBox()
	} else {
		bbox = geojson.BBox{Min: geojson.Position{X: minLon, Y: minLat, Z: minZ}, Max: geojson.Position{X: maxLon, Y: maxLat, Z: maxZ}}
	}
	var bboxes []geojson.BBox
	if sparse > 0 {
		split := 1 << sparse
		xpart := (bbox.Max.X - bbox.Min.X) / float64(split)
		ypart := (bbox.Max.Y - bbox.Min.Y) / float64(split)
		for y := bbox.Min.Y; y < bbox.Max.Y; y += ypart {
			for x := bbox.Min.X; x < bbox.Max.X; x += xpart {
				bboxes = append(bboxes, geojson.BBox{
					Min: geojson.Position{X: x, Y: y, Z: minZ},
					Max: geojson.Position{X: x + xpart, Y: y + ypart, Z: maxZ},
				})
			}
		}
		for _, bbox := range bboxes {
			if obj != nil {
				c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
					if o.Intersects(obj) {
						if iterator(id, o, fields) {
							return false
						}
					}
					return true
				})
			}
			c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
				if o.IntersectsBBox(bbox) {
					if iterator(id, o, fields) {
						return false
					}
				}
				return true
			})
		}
		return 0
	}
	if obj != nil {
		return c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
			if o.Intersects(obj) {
				return iterator(id, o, fields)
			}
			return true
		})
	}
	return c.geoSearch(cursor, bbox, func(id string, o geojson.Object, fields []float64) bool {
		if o.IntersectsBBox(bbox) {
			return iterator(id, o, fields)
		}
		return true
	})
}

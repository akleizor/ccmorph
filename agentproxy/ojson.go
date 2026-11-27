package agentproxy

import "github.com/akleizor/ccmorph/ordjson"

// A body the proxy rewrites is an ordjson document, so it goes on in the key
// order the session wrote it. These read one typed field of an object.

func field(o *ordjson.Obj, k string) any {
	v, _ := o.Get(k)
	return v
}

func str(o *ordjson.Obj, k string) (string, bool) {
	v, _ := o.Get(k)
	s, ok := v.(string)
	return s, ok
}

func child(o *ordjson.Obj, k string) *ordjson.Obj {
	v, _ := o.Get(k)
	c, _ := v.(*ordjson.Obj)
	return c
}

func list(o *ordjson.Obj, k string) []any {
	v, _ := o.Get(k)
	l, _ := v.([]any)
	return l
}

// del removes k; false when it was not there.
func del(o *ordjson.Obj, k string) bool {
	if !o.Has(k) {
		return false
	}
	o.Delete(k)
	return true
}

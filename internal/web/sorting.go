package web

import (
	"html/template"
	"net/url"
)

// sortHeader is one sortable table column header.
type sortHeader struct {
	Label    string
	Href     template.URL
	AriaSort string // "ascending", "descending" or "" for unsorted
}

// sortHeaders builds header links that keep the current query (filters) and
// toggle the direction of the active column, like the legacy list pages.
func sortHeaders(path string, q url.Values, sort string, desc bool, cols [][2]string) []sortHeader {
	out := make([]sortHeader, 0, len(cols))
	for _, c := range cols {
		key, label := c[0], c[1]
		v := url.Values{}
		for k, vs := range q {
			if k != "sort" && k != "dir" && k != "flash" && k != "n" {
				v[k] = vs
			}
		}
		v.Set("sort", key)
		h := sortHeader{Label: label}
		if key == sort {
			if desc {
				h.AriaSort = "descending"
			} else {
				h.AriaSort = "ascending"
				v.Set("dir", "desc")
			}
		}
		h.Href = template.URL(path + "?" + v.Encode())
		out = append(out, h)
	}
	return out
}

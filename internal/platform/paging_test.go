package platform

import (
	"net/http/httptest"
	"testing"
)

func TestPagesCapsLargePage(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/users?page=1000001&page_size=100", nil)
	size, offset := pages(r)
	if size != 100 || offset != 99_999_900 {
		t.Fatalf("pages returned size=%d offset=%d", size, offset)
	}
}

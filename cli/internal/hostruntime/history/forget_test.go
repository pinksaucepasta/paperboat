package history

import "testing"

func TestForgetDropsOnlyThatAttachment(t *testing.T) {
	h, err := New(1 << 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Acknowledge("att_a", 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Acknowledge("att_b", 0); err != nil {
		t.Fatal(err)
	}
	h.Forget("att_a")
	if _, ok := h.Acknowledged("att_a"); ok {
		t.Fatal("forgotten attachment is still acknowledged")
	}
	if _, ok := h.Acknowledged("att_b"); !ok {
		t.Fatal("Forget dropped another attachment")
	}
}

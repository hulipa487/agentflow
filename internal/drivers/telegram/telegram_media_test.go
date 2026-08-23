package telegram

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
)

type captureSink struct {
	inbs []router.Inbound
}

func (c *captureSink) Submit(in router.Inbound) { c.inbs = append(c.inbs, in) }

// newMediaMockTG serves getFile + the file download endpoint, like Telegram.
func newMediaMockTG(t *testing.T, fileBody string) (*mockTG, *captureSink, *media.Store) {
	t.Helper()
	m := &mockTG{}
	sink := &captureSink{}
	mux := http.NewServeMux()
	mux.HandleFunc("/botTEST/getFile", func(w http.ResponseWriter, r *http.Request) {
		fid := r.URL.Query().Get("file_id")
		if fid != "PHOTO1" {
			_, _ = w.Write([]byte(`{"ok":false,"description":"bad file_id"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"PHOTO1","file_path":"photos/big.jpg"}}`))
	})
	mux.HandleFunc("/file/botTEST/photos/big.jpg", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fileBody))
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	m.base = m.srv.URL
	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m, sink, store
}

func mediaDriver(m *mockTG, sink *captureSink, store *media.Store) *Driver {
	return &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		apiBase: m.base,
		store:   store,
		pol:     media.Policy{Allow: []string{"image/*"}},
		sink:    sink,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func photoUpdate() update {
	u := update{UpdateID: 42}
	u.Message = &tgMessage{
		MessageID: 1,
		Caption:   "look at this",
		Text:      "",
	}
	u.Message.From.ID = 7
	u.Message.Chat.ID = 99
	u.Message.Photo = []photoSize{
		{FileID: "SMALL", Width: 100, Height: 100},
		{FileID: "PHOTO1", Width: 1280, Height: 720},
	}
	return u
}

func TestPhotoIngestStoresBlobAndAttachesPart(t *testing.T) {
	m, sink, store := newMediaMockTG(t, "jpegbytes")
	d := mediaDriver(m, sink, store)

	d.handleUpdate(photoUpdate())

	if len(sink.inbs) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(sink.inbs))
	}
	msg := sink.inbs[0].Message
	if msg.Text != "look at this" {
		t.Fatalf("caption should flow as text, got %q", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Type != "image" || att.MIME != "image/jpeg" {
		t.Fatalf("bad attachment: %+v", att)
	}
	if !media.ValidHandle(att.Handle) {
		t.Fatalf("bad handle %q", att.Handle)
	}
	b, err := store.ReadAll(att.Handle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "jpegbytes" {
		t.Fatalf("stored content %q", b)
	}
}

func TestMediaPolicyDisabledDropsMediaKeepsCaption(t *testing.T) {
	m, sink, store := newMediaMockTG(t, "jpegbytes")
	d := mediaDriver(m, sink, store)
	d.store = nil // no policy wired
	d.pol = media.Policy{}

	d.handleUpdate(photoUpdate())

	if len(sink.inbs) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(sink.inbs))
	}
	msg := sink.inbs[0].Message
	if msg.Text != "look at this" {
		t.Fatalf("caption should still flow, got %q", msg.Text)
	}
	if len(msg.Attachments) != 0 {
		t.Fatalf("expected no attachments, got %d", len(msg.Attachments))
	}
}

func TestMediaPolicyRejectsDisallowedMime(t *testing.T) {
	m, sink, store := newMediaMockTG(t, "pdfbytes")
	d := mediaDriver(m, sink, store)
	d.pol = media.Policy{Allow: []string{"image/*"}}

	u := photoUpdate()
	u.Message.Photo = nil
	u.Message.Doc = &struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MIME     string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	}{FileID: "DOC1", FileName: "x.pdf", MIME: "application/pdf"}
	u.Message.Caption = "the doc"
	d.handleUpdate(u)

	if len(sink.inbs) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(sink.inbs))
	}
	if len(sink.inbs[0].Message.Attachments) != 0 {
		t.Fatal("pdf should be dropped under image-only policy")
	}
	if sink.inbs[0].Message.Text != "the doc" {
		t.Fatalf("caption should flow, got %q", sink.inbs[0].Message.Text)
	}
}

func TestMediaOnlyMessageStillFlows(t *testing.T) {
	m, sink, store := newMediaMockTG(t, "jpegbytes")
	d := mediaDriver(m, sink, store)

	u := photoUpdate()
	u.Message.Caption = "" // no caption at all
	d.handleUpdate(u)

	if len(sink.inbs) != 1 {
		t.Fatal("media-only message must flow (old code dropped empty-text messages)")
	}
	if len(sink.inbs[0].Message.Attachments) != 1 {
		t.Fatal("expected the photo attachment")
	}
}

func TestDeliverPhotoMultipart(t *testing.T) {
	var gotMethod, gotForm string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = strings.TrimPrefix(r.URL.Path, "/botTEST/")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		gotForm = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(strings.NewReader("imgdata"), "image/png", media.Policy{Allow: []string{"image/*"}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		apiBase: srv.URL,
		store:   store,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	err = d.Deliver("99", "a caption", []media.Part{{Type: "image", MIME: "image/png", Handle: ref.Handle, Name: "pic.png"}})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotMethod != "sendPhoto" {
		t.Fatalf("method %q", gotMethod)
	}
	if !strings.Contains(gotForm, "multipart/form-data") {
		t.Fatalf("content-type %q", gotForm)
	}
	body := string(gotBody)
	for _, want := range []string{
		`name="chat_id"`,
		`name="caption"`,
		"a caption",
		"imgdata",
		"pic.png",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("multipart body missing %q", want)
		}
	}
}

func TestDeliverTextOnlyUsesSendMessage(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = strings.TrimPrefix(r.URL.Path, "/botTEST/")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	d := &Driver{name: "telegram", token: "TEST", apiBase: srv.URL, log: testLogger(), client: &http.Client{Timeout: 5 * time.Second}}
	if err := d.Deliver("99", "hello", nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotMethod != "sendMessage" {
		t.Fatalf("method %q", gotMethod)
	}
}

// Compile-time interface check (guard against signature drift).
var _ router.Sink = (*captureSink)(nil)

package transport

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestClosePipeAfterCopy(t *testing.T) {
	forkCount := 3
	stream := []byte("zażółć gęślą jaźń\r\n")
	writer, readers := multiplicateReadClosers(forkCount)
	go func() {
		_, err := writer.Write(stream)
		if err != nil {
			t.Error("Cannot write to multiwriter")
		}
		for _, r := range readers {
			pr := r.(*io.PipeReader)
			err := pr.CloseWithError(io.EOF)
			if err != nil {
				t.Log("io pkg broke some promisses :/")
			}
		}
	}()
	for _, r := range readers {
		p := make([]byte, len(stream))
		n, err := io.ReadFull(r, p)
		if n < len(stream) {
			t.Errorf("Read full read only %d bytes and returned Error %s", n, err.Error())
		}
		if err != nil {
			t.Logf("%q", err.Error())
		}
	}

}

func TestLimitReaderFromBuffer(t *testing.T) {
	stream := []byte("some text")
	reader := bytes.NewBuffer(stream)
	lreader := io.LimitReader(reader, int64(len(stream)))
	p := make([]byte, len(stream))
	n, err := io.ReadFull(lreader, p)
	if n == 0 {
		t.Error("read 0 bytes")
	}
	if err != nil && err != io.EOF {
		t.Errorf("Got strange error %q", err)
	}
}

func dummyReq(stream []byte, addContentLength int64) *http.Request {
	reader := bytes.NewBuffer(stream)
	limit := int64(len(stream))
	req, _ := http.NewRequest(
		"POST",
		"http://example.com/index",
		io.LimitReader(reader, limit))
	req.ContentLength = limit + addContentLength
	return req
}

func TestPipeReads(t *testing.T) {
	// Check if we may replicate reader into more readers
	forkCount := 3
	stream := []byte("zażółć gęślą jaźń\r\n")
	writer, readers := multiplicateReadClosers(forkCount)
	if len(readers) != forkCount {
		t.Errorf("Expected %d readers got %d", forkCount, len(readers))
	}
	go func() {
		_, err := writer.Write(stream)
		if err != nil {
			t.Error("Cannot write to stream")
		}
	}()
	for _, reader := range readers {
		p := make([]byte, len(stream))
		_, err := io.ReadFull(reader, p)
		if err != nil {
			t.Error(err)
		}
		if !bytes.Equal(stream, p) {
			t.Errorf("Expected same readings as writes got %q", p)
		}
	}
}

func mkDummySrvs(count int, stream []byte, t *testing.T) []*url.URL {
	urls := make([]*url.URL, 0, count)
	dummySrvs := make([]*httptest.Server, 0, count)
	for i := 0; i < count; i++ {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := make([]byte, r.ContentLength)
			_, err := io.ReadFull(r.Body, p)
			if err != nil {
				t.Logf("Err %q", err.Error())
			}
			<-time.After(10 * time.Millisecond)
			if bytes.Equal(stream, p) {
				return
			}
		}))
		dummySrvs = append(dummySrvs, ts)
		urlN, err := url.Parse(ts.URL)
		if err != nil {
			t.Error(err)
		}
		urls = append(urls, urlN)
	}
	return urls
}

func mkTransport(urls []*url.URL, t *testing.T) *MultiTransport {
	return &MultiTransport{
		RoundTripper: http.DefaultTransport,
		Backends:     urls,
		HandleResponses: func(in <-chan *ReqResErrTuple) *ReqResErrTuple {
			out := make(chan *ReqResErrTuple, 1)
			sent := false
			for {
				rs, ok := <-in
				if !ok {
					break
				}
				if rs.Err == nil {
					b := make([]byte, 3)
					_, err := rs.Res.Body.Read(b)
					if err != nil && err != io.EOF {
						t.Errorf("Body reading error %q", err)
					}
					if bytes.HasPrefix(b, []byte("ERR")) {
						t.Error("Body has error")
					}
				}
				if !sent {
					out <- rs
					sent = true
				}

			}
			return <-out
		}}
}

func TestTimeoutReader(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 4; i++ {
			_, err := pw.Write([]byte("some string"))
			if err != nil {
				t.Error(err.Error())
			}
			<-time.After(100 * time.Millisecond)
		}
	}()
	tr := &TimeoutReader{pr, time.Second * 2}
	for i := 0; i < 4; i++ {
		_, err := tr.Read(make([]byte, 20))
		if err != nil {
			t.Errorf("Timeout was not reached, but error occured %s", err.Error())
		}
	}
	tr2 := &TimeoutReader{pr, time.Millisecond}
	_, err := tr2.Read(make([]byte, 0, 20))
	if err != ErrTimeout {
		t.Errorf("Should return an err")
	}
}

func TestRequestMultiplication(t *testing.T) {
	stream := []byte("zażółć gęślą jaźń")
	urls := mkDummySrvs(3, stream, t)
	req := dummyReq(stream, 0)
	transp := mkTransport(urls, t)
	_, err := transp.RoundTrip(req)
	if err != nil {
		t.Errorf("RoundTrip err")
	}
	req2 := dummyReq(stream, 1)
	_, err2 := transp.RoundTrip(req2)
	if err2 == nil {
		t.Errorf("Should get ErrTimeout or ErrBodyContentLengthMismatch")
	}
}

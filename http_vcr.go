package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
)

type HttpVcr struct {
	recorder   *VcrRecorder
	replayer   *VcrReplayer
	recordMode bool
	remoteURL  string // base URL of remote without trailing `/`
}

func NewHttpVcr(filepath string, recordMode bool, remoteURL string) (vcr HttpVcr, err error) {
	vcr = HttpVcr{}

	if recordMode {
		// delete file if exists
		if _, err := os.Stat(filepath); !os.IsNotExist(err) {
			err = os.Remove(filepath)
			if err != nil {
				return vcr, err
			}
		}

		recorder, e := NewRecorder(filepath)
		vcr.recorder = recorder
		err = e
	} else {
		// delete file if exists
		if _, err := os.Stat(filepath); os.IsNotExist(err) {
			return vcr, err
		}

		sequentialComparator := func(req1 interface{}, req2 interface{}) (accept bool, shortCircuitNow bool) {
			return true, true
		}

		replayer, e := NewReplayer(filepath, HttpRequestSerializable{}, HttpResponseSerializable{}, sequentialComparator)
		vcr.replayer = replayer
		err = e
	}

	vcr.recordMode = recordMode
	vcr.remoteURL = remoteURL
	return vcr, err
}

func handleErrorInHandler(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	w.WriteHeader(500)
	// TODO: should we crash the program or keep going?
	fmt.Println("\n<-- 500 error: ", err)
}

func (httpVcr *HttpVcr) handler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("\n--> %v to %v", r.Method, r.RequestURI)

	// --- pass to VCR, get response back
	var resp *http.Response
	var err error
	if httpVcr.recordMode {
		resp, err = httpVcr.getResponseFromRemote(r)
		if err != nil {
			handleErrorInHandler(w, err)
			return
		}
		fmt.Printf("\n<-- %v from %v\n", resp.Status, "REMOTE")
	} else {
		resp, err = httpVcr.getNextRecordedCassetteResponse(r)
		if err != nil {
			handleErrorInHandler(w, err)
			return
		}
		fmt.Printf("\n<-- %v from %v\n", resp.Status, "CASSETTE")
	}
	defer resp.Body.Close() // we need to close the body

	// take response and write the httpResponse
	// TODO: this is kind of a piecemeal way to transfer data from the proxied response
	// 		 Is there a way to copy and return the entire proxied response? (and not worry about missing a field)
	w.WriteHeader(resp.StatusCode)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		handleErrorInHandler(w, err)
		return
	}

	io.Copy(w, bytes.NewBuffer(bodyBytes)) // TODO: there is an ordering bug between this and recorder.Write() below

	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	if httpVcr.recordMode {
		err = httpVcr.recorder.Write(NewSerializableHttpRequest(r), NewSerializableHttpResponse(resp))
		if err != nil {
			handleErrorInHandler(w, err)
			return
		}
	}
}

func (httpVcr *HttpVcr) InitializeServer(address string) *http.Server {
	customMux := http.NewServeMux()
	server := &http.Server{Addr: address, Handler: customMux}

	// --- VCR control handlers
	// TODO: only stops recording, does not shutdown the server
	customMux.HandleFunc("/vcr/stop", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println()
		fmt.Println("Received /vcr/stop. Stopping...")

		httpVcr.recorder.Close()
	})

	// --- Default VCR catch-all handler
	customMux.HandleFunc("/", httpVcr.handler)

	return server
}

// TODO: currently has issues - do manually for now
func generateSelfSignedCertificates() error {
	gorootPath := os.Getenv("GOROOT")
	fmt.Println("GOROOT: ", gorootPath)
	certGenerationScript := gorootPath + "/src/crypto/tls/generate_cert.go"
	rsaBits := "2048"
	host := "localhost, 127.0.0.1"
	startDate := "Jan 1 00:00:00 1970"
	duration := "--duration=100000h"

	cmd := exec.Command("go", "run", certGenerationScript, "--rsa-bits", rsaBits, "--host", host, "--ca", "--start-date", startDate, duration)
	// cmd := exec.Command("go env")
	// cmd := exec.Command("ls")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("generating certs failed: %w", err)
	} else {
		return nil
	}
}

func main() {
	filepath := "main_result.yaml"
	addressString := "localhost:8080"
	recordMode := true
	remoteURL := "https://api.stripe.com"
	// remoteURL := "https://gobyexample.com"

	httpVcr, err := NewHttpVcr(filepath, recordMode, remoteURL)
	check(err)

	fmt.Println()
	fmt.Printf("===\nUsing cassette \"%v\".\nListening via HTTPS on %v\nRecordMode: %v\n===", filepath, addressString, recordMode)

	fmt.Println()

	server := httpVcr.InitializeServer(addressString)

	log.Fatal(server.ListenAndServeTLS("cert.pem", "key.pem"))
}

func (httpVcr *HttpVcr) getResponseFromRemote(request *http.Request) (resp *http.Response, err error) {
	// TODO: placeholder proxy a request to some random website. Later - this should pass on the request
	// We need to pass on the entire request (or at least the Authorization part of the header)

	client := &http.Client{}
	req, err := http.NewRequest(request.Method, httpVcr.remoteURL+request.RequestURI, nil)
	req.Header.Add("Authorization", request.Header.Get("Authorization"))

	res, err := client.Do(req)
	// res, err := http.Get(remoteUrl + request.URL.RequestURI())

	if err != nil {
		return nil, err
	}

	// If returning the response to code that expects to read it, we cannot call res.Body.Close() here.
	// defer res.Body.Close()

	return res, nil
}

// returns error if something doesn't match the cassette
func (httpVcr *HttpVcr) getNextRecordedCassetteResponse(request *http.Request) (resp *http.Response, err error) {
	// the passed in request arg may not be necessary

	responseWrapper, err := httpVcr.replayer.Write(NewSerializableHttpRequest(request))
	if err != nil {
		return &http.Response{}, err
	}

	response := (*responseWrapper).(*http.Response)

	return response, err
}

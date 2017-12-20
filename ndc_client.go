package ndc

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/AirGateway/ndc-go-sdk/logger"
	"golang.org/x/net/html"

	"bufio"
	"bytes"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

var NDCSupportedMethods = map[string]struct{}{
	"AirShoppingRQ":      {},
	"FlightPriceRQ":      {},
	"SeatAvailabilityRQ": {},
	"ServiceListRQ":      {},
	"ServicePriceRQ":     {},
	"OrderCreateRQ":      {},
	"OrderRetrieveRQ":    {},
	"OrderListRQ":        {},
	"OrderCancelRQ":      {},
	"ItinReshopRQ":       {},
}

var TemplateVars = []string{"request_name"}

type Extras struct {
	Value map[string]string
}
type ClientOptions struct {
	ConfigPath string
}
type AsynchArgs struct {
	Callback       func(string, AsynchArgs)
	HeaderCallback func(http.Header) error
	Out            http.ResponseWriter
	Flusher        http.Flusher
	WG             *sync.WaitGroup
	InitTime       time.Time
	Limit          int
}
type Client struct {
	Options         ClientOptions
	HasTemplateVars bool
	Extras          map[string]Extras
	Config          map[string]yaml.MapSlice
	RawConfig       []byte
	HttpClient      *http.Client
}

var responseDelimiter = []byte("<!-- AG-EOM -->")

func NewClient(options *ClientOptions, extras map[string]Extras) (*Client, error) {
	client := &Client{Options: *options}
	client.Config = map[string]yaml.MapSlice{}
	client.Extras = extras
	client.HttpClient = &http.Client{}
	client.HasTemplateVars = false
	err := client.LoadConfig()
	return client, err
}

func ConfigHasTemplateVars(RawConfig *[]byte) int {
	config := string(*RawConfig)
	var matches = 0
	for _, VarName := range TemplateVars {
		VarIndex := strings.Index(config, VarName)
		if VarIndex > 0 {
			matches++
		}
	}
	return matches
}

func MapSliceToMap(slice yaml.MapSlice, m map[string]interface{}) map[string]interface{} {
	if m == nil {
		m = make(map[string]interface{})
	}
	for _, entry := range slice {
		switch entry.Value.(type) {
		case yaml.MapSlice:
			m[fmt.Sprint(entry.Key)] = MapSliceToMap(entry.Value.(yaml.MapSlice), nil)
		default:
			m[fmt.Sprint(entry.Key)] = fmt.Sprint(entry.Value)
		}
	}

	return m
}

func (client *Client) LoadConfig() error {
	config, err := ioutil.ReadFile(client.Options.ConfigPath)

	client.RawConfig = config
	err = yaml.Unmarshal(client.RawConfig, &client.Config)

	if ConfigHasTemplateVars(&client.RawConfig) > 0 {
		client.HasTemplateVars = true
	}

	return err
}

func (client *Client) PrepareConfig(message Message) (Config map[string]interface{}) {

	ModifiedConfig := string(client.RawConfig)

	for _, VarName := range TemplateVars {

		var VarValue = ""

		switch VarName {
		case "request_name":
			VarValue = message.Method

		}

		VarName = fmt.Sprintf("{{%s}}", VarName)

		ModifiedConfig = strings.Replace(ModifiedConfig, VarName, VarValue, -1)
	}

	ConfigMapSlice := yaml.MapSlice{}
	yaml.Unmarshal([]byte(ModifiedConfig), &ConfigMapSlice)

	Config = MapSliceToMap(ConfigMapSlice, nil)

	return
}

func (client *Client) AppendHeaders(r *http.Request, HeadersConfig interface{}) {
	headers := HeadersConfig.(map[string]interface{})
	for Header, Value := range headers {
		r.Header.Add(Header, Value.(string))
	}
}

func (client *Client) RequestAsynch(message Message, args AsynchArgs) {
	fmt.Println("*** RequestAsynch")
	Response := client.Request(message)

	// Handle NDC response headers if callback defined
	if args.HeaderCallback != nil {
		if err := args.HeaderCallback(Response.Header); err != nil {
			fmt.Println("HeaderCallback error: ", err)
		}
	}

	message_aux := ""
	reader := bufio.NewReader(Response.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err == nil {
			message_aux = message_aux + string(line)
			if strings.Contains(message_aux, "<!-- AG-EOM -->") {
				// fmt.Println("*** callback", message_aux)
				fmt.Println("*** callback")
				args.WG.Add(1)
				go args.Callback(message_aux, args)
				// debug: print response
				fmt.Println(Prettify(message_aux, "    "))

				message_aux = ""
			}
		} else {
			fmt.Println("ERROR", err)
			break
		}
	}
	args.WG.Wait()
	fmt.Println("*** RequestAsynch finishes")
}

func (client *Client) RequestAsync(ctx context.Context, message Message) (*http.Response, chan []byte) {
	fmt.Println("*** RequestAsync")
	response := client.Request(message)
	if response == nil {
		return nil, nil
	}
	offersChannel := make(chan []byte)
	go func() {
		// Make sure body closed on return
		defer response.Body.Close()
		// Prepare reader line by line
		reader := bufio.NewReader(response.Body)
		buf := bytes.NewBuffer([]byte(""))
	For:
		for {
			select {
			// Stop if passed context cancelled
			case <-ctx.Done():
				break For
			// Handle offers
			default:
				ln, err := reader.ReadBytes('\n')
				if err != nil || err == io.EOF {
					break For
				}
				buf.Write(ln)
				if bytes.Contains(ln, responseDelimiter) {
					offersChannel <- buf.Bytes()
					buf.Reset()
				}
			}
		}
		close(offersChannel)
		return
	}()
	return response, offersChannel
}

func (client *Client) RequestAsync2(message Message, responseChannel chan []byte) {
	fmt.Println("*** RequestAsynch2")
	response := client.Request(message)
	reader := bufio.NewReader(response.Body)
	buf := bytes.NewBuffer([]byte(""))
	for {
		ln, err := reader.ReadBytes('\n')
		if err != nil || err == io.EOF {
			break
		}
		buf.Write(ln)
		if bytes.Contains(ln, responseDelimiter) {
			responseChannel <- buf.Bytes()
			buf.Reset()
		}
	}
	close(responseChannel)
}

func (client *Client) RequestSync(message Message) (string, http.Header) {
	fmt.Println("-> Doing Request:\n---\n")
	Response := client.Request(message)
	fmt.Println("-> Receiving response:\n---\n")
	//fmt.Println( Response , "\n---\n-> Response body:\n---\n")
	body_, _ := ioutil.ReadAll(Response.Body)
	//fmt.Printf("%v\n", formatResponse(Response))
	buf, _ := Prettify(string(body_[:]), "    ")
	fmt.Println(buf)
	return string(body_), Response.Header
}

func (client *Client) RequestSynch(message Message) string {
	fmt.Println("-> Doing Request:\n---\n")
	Response := client.Request(message)
	fmt.Println("-> Receiving response:\n---\n")
	//fmt.Println( Response , "\n---\n-> Response body:\n---\n")
	body_, _ := ioutil.ReadAll(Response.Body)
	//fmt.Printf("%v\n", formatResponse(Response))
	buf, _ := Prettify(string(body_[:]), "    ")
	fmt.Println(buf)
	return string(body_)
}

func (client *Client) Request(message Message) *http.Response {
	logger := logger.Get()

	var Config, ServerConfig, RestConfig map[string]interface{}
	//var Config, RestConfig map[string]interface{}

	message.Client = client

	body, _ := message.Prepare()

	if client.HasTemplateVars {
		Config = client.PrepareConfig(message)
	} else {
		//Config = client.Config
	}

	//fmt.Println(Config)
	RestConfig = Config["rest"].(map[string]interface{})
	ServerConfig = Config["server"].(map[string]interface{})
	var RequestURL interface{}
	if env, ok := client.Extras["enviroment"]; ok == true {
		RequestURL = ServerConfig["url_"+env.Value["url"]]
	} else {
		RequestURL = ServerConfig["url_prod"]
	}
	RequestReader := bytes.NewReader(body)

	Request, _ := http.NewRequest("POST", RequestURL.(string), RequestReader)
	client.AppendHeaders(Request, RestConfig["headers"])
	//elem, ok := client.Extras["headers"]
	if headers, ok := client.Extras["headers"]; ok == true {
		for Header, Value := range headers.Value {
			//fmt.Println(Header, Value)
			Request.Header.Del(Header)
			Request.Header.Add(Header, Value)
		}
	}
	// Debug: print request
	fmt.Printf("%v\n", formatRequest(Request))

	Response, err := client.HttpClient.Do(Request)
	if err != nil {
		logger.Error()
	}
	return Response
}

func convert(b []byte) string {
	s := make([]string, len(b))
	for i := range b {
		s[i] = strconv.Itoa(int(b[i]))
	}
	return strings.Join(s, ",")
}

// formatRequest generates ascii representation of a http request
func formatRequest(r *http.Request) string {
	// Create return string
	var request []string

	// Add the request string
	url := fmt.Sprintf("%v %v %v", r.Method, r.URL, r.Proto)
	request = append(request, url)

	// Add the host
	request = append(request, fmt.Sprintf("Host: %v", r.Host))

	// Loop through headers
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			request = append(request, fmt.Sprintf("%v: %v", name, h))
		}
	}

	// Get the request body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("Error reading body: %v", err)
	}
	request = append(request, fmt.Sprintf("Body:\n%s", body))
	// Restore body into Request
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	// If this is a POST, add post data
	if r.Method == "POST" {
		r.ParseForm()
		request = append(request, "\n")
		request = append(request, r.Form.Encode())
	}

	// write request end
	request = append(request, fmt.Sprintf("**** end of request"))

	// Return the request as a string
	return strings.Join(request, "\n")
}

// formatResponse generates ascii representation of a http response
func formatResponse(r *http.Response) string {
	// Create return string
	var response []string

	// Add the response string
	url := fmt.Sprintf("%v", r.Proto)
	response = append(response, url)

	// Loop through headers
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			response = append(response, fmt.Sprintf("%v: %v", name, h))
		}
	}

	// Get the response body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("Error reading body: %v", err)
	}
	_body, _ := Prettify(string(body[:]), "    ")
	response = append(response, fmt.Sprintf("Body:\n%s", _body))

	// Restore body into Request
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	// write request end
	response = append(response, fmt.Sprintf("**** end of response"))

	// Return the request as a string
	return strings.Join(response, "\n")
}

// Prettify prints readable XML
// From: https://stackoverflow.com/questions/21117161/go-how-would-you-pretty-print-prettify-html#23285911
func Prettify(raw string, indent string) (pretty string, e error) {
	// Temporary disable Prettify as it takes too much time
	return raw, nil

	r := strings.NewReader(raw)
	z := html.NewTokenizer(r)
	pretty = ""
	depth := 0
	prevToken := html.CommentToken
	for {
		tt := z.Next()
		tokenString := string(z.Raw())

		// strip away newlines
		if tt == html.TextToken {
			stripped := strings.Trim(tokenString, "\n")
			if len(stripped) == 0 {
				continue
			}
		}

		if tt == html.EndTagToken {
			depth -= 1
		}

		if tt != html.TextToken {
			if prevToken != html.TextToken {
				pretty += "\n"
				for i := 0; i < depth; i++ {
					pretty += indent
				}
			}
		}

		pretty += tokenString

		// last token
		if tt == html.ErrorToken {
			break
		} else if tt == html.StartTagToken {
			depth += 1
		}
		prevToken = tt
	}
	return strings.Trim(pretty, "\n"), nil
}

package main

import (
	//	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/storage"
	"context"
	"encoding/json"
	"fmt"
	"github.com/auth0/go-jwt-middleware"
	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"
	"github.com/pborman/uuid"
	elastic "gopkg.in/olivere/elastic.v3"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
)

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Post struct {
	User     string   `json:"user"`
	Message  string   `json:"message"`
	Location Location `json:"location"`
	Url      string   `json:"url"`
	Type     string   `json:"type"`
	Face     float64  `json:"face"`
}

const (
	INDEX    = "around"
	TYPE     = "post"
	DISTANCE = "200km"
	// Needs to update this URL if you deploy it to cloud.
	ES_URL      = "http://34.74.217.223:9200"
	BUCKET_NAME = "post-images-240220"
	PROJECT_ID  = "single-bindery-240220"
	BT_INSTANCE = "around-post"
	API_PREFIX  = "/api/v1"
)

var mySigningKey = []byte("secret")

var (
	mediaTypes = map[string]string{
		".jpeg": "image",
		".jpg":  "image",
		".gif":  "image",
		".png":  "image",
		".mov":  "video",
		".mp4":  "video",
		".avi":  "video",
		".flv":  "video",
		".wmv":  "video",
	}
)

func main() {

	// Create a client
	client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
	if err != nil {
		panic(err)
		return
	}

	// Use the IndexExists service to check if a specified index exists.
	exists, err := client.IndexExists(INDEX).Do()
	if err != nil {
		panic(err)
	}
	if !exists {
		// Create a new index.
		mapping := `{
			"mappings":{
				"post":{
					"properties":{
						"location":{
							"type":"geo_point"
						}
					}
				}
			}
		}`
		_, err := client.CreateIndex(INDEX).Body(mapping).Do()
		if err != nil {
			// Handle error
			panic(err)
		}
	}
	fmt.Println("started-service")

	r := mux.NewRouter()

	var jwtMiddleware = jwtmiddleware.New(jwtmiddleware.Options{
		ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
			return mySigningKey, nil
		},
		SigningMethod: jwt.SigningMethodHS256,
	})

	r.Handle(API_PREFIX+"/post", jwtMiddleware.Handler(http.HandlerFunc(handlerPost)))
	r.Handle(API_PREFIX+"/search", jwtMiddleware.Handler(http.HandlerFunc(handlerSearch)))
	r.Handle(API_PREFIX+"/cluster", jwtMiddleware.Handler(http.HandlerFunc(handlerCluster)))
	r.Handle(API_PREFIX+"/login", http.HandlerFunc(loginHandler))
	r.Handle(API_PREFIX+"/signup", http.HandlerFunc(signupHandler))

	// Backend endpoints.
	http.Handle(API_PREFIX+"/", r)

	log.Fatal(http.ListenAndServe(":8080", nil))

}

func handlerPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")

	user := r.Context().Value("user")
	claims := user.(*jwt.Token).Claims
	username := claims.(jwt.MapClaims)["username"]

	r.ParseMultipartForm(32 << 20)

	//Parse from data
	fmt.Println("Recived one post request %s\n", r.FormValue("message"))
	lat, _ := strconv.ParseFloat(r.FormValue("lat"), 64)
	lon, _ := strconv.ParseFloat(r.FormValue("lon"), 64)

	p := &Post{
		User:    username.(string),
		Message: r.FormValue("message"),
		Location: Location{
			Lat: lat,
			Lon: lon,
		},
	}

	id := uuid.New()

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image is not available", http.StatusInternalServerError)
		fmt.Printf("Image is not available %v.\n", err)
		panic(err)
	}
	defer file.Close()

	im, header, _ := r.FormFile("image")
	defer im.Close()
	suffix := filepath.Ext(header.Filename)

	// Client needs to know the media type so as to render it.
	if t, ok := mediaTypes[suffix]; ok {
		p.Type = t
	} else {
		p.Type = "unknown"
	}
	// ML Engine only supports jpeg.
	if suffix == ".jpeg" {
		if score, err := annotate(im); err != nil {
			http.Error(w, "Failed to annotate the image", http.StatusInternalServerError)
			fmt.Printf("Failed to annotate the image %v\n", err)
			return
		} else {
			p.Face = score
		}
	}
	fmt.Printf("Add face=%v\n", p.Face)

	ctx := context.Background()

	_, attrs, err := saveToGCS(ctx, file, BUCKET_NAME, id)
	if err != nil {
		http.Error(w, "GCS is not setup", http.StatusInternalServerError)
		fmt.Printf("GCS is not setup %v.\n", err)
		panic(err)
	}

	p.Url = attrs.MediaLink

	//Save to ES
	saveToES(p, id)

	//Save to bigTable
	//saveToBigTable(p, id)
}

func saveToGCS(ctx context.Context, r io.Reader, bucketName, name string) (*storage.ObjectHandle, *storage.ObjectAttrs, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer client.Close()

	bucket := client.Bucket(bucketName)

	if _, err := bucket.Attrs(ctx); err != nil {
		return nil, nil, err
	}

	obj := bucket.Object(name)
	wc := obj.NewWriter(ctx)
	if _, err = io.Copy(wc, r); err != nil {
		return nil, nil, err
	}
	if err := wc.Close(); err != nil {
		return nil, nil, err
	}
	if err := obj.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return nil, nil, err
	}

	attrs, err := obj.Attrs(ctx)
	fmt.Printf("Post is saved to GCS: %s\n", attrs.MediaLink)

	return obj, attrs, err
}

/*
func saveToBigTable(p *Post, id string) {

	ctx := context.Background()
	// you must update project name here
	bt_client, err := bigtable.NewClient(ctx, PROJECT_ID, BT_INSTANCE)
	if err != nil {
		panic(err)
		return
	}

	tbl := bt_client.Open("post")
	mut := bigtable.NewMutation()
	t := bigtable.Now()

	mut.Set("post", "user", t, []byte(p.User))
	mut.Set("post", "message", t, []byte(p.Message))
	mut.Set("location", "lat", t, []byte(strconv.FormatFloat(p.Location.Lat, 'f', 1, 64)))
	mut.Set("location", "lon", t, []byte(strconv.FormatFloat(p.Location.Lon, 'f', 1, 64)))

	err = tbl.Apply(ctx, id, mut)
	if err != nil {
		panic(err)
		return
	}
	fmt.Printf("Post is saved to BigTable: %s\n", p.Message)

}
*/

func saveToES(p *Post, id string) {
	es_client, err := elastic.NewClient(elastic.SetURL(ES_URL),
		elastic.SetSniff(false))
	if err != nil {
		panic(err)
	}

	_, err = es_client.Index().
		Index(INDEX).
		Type(TYPE).
		Id(id).
		BodyJson(p).
		Refresh(true).
		Do()
	if err != nil {
		panic(err)
	}

	fmt.Printf("Post is saved to index: %s\n", p.Message)
}

func handlerCluster(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one request for clustering.")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")

	if r.Method != "GET" {
		return
	}
	term := r.URL.Query().Get("term")

	// Create a client
	client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
	if err != nil {
		http.Error(w, "ES is not setup", http.StatusInternalServerError)
		fmt.Printf("ES is not setup %v\n", err)
		return
	}

	q := elastic.NewRangeQuery(term).Gte(0.8)

	searchResult, err := client.Search().
		Index(INDEX).
		Query(q).
		Pretty(true).
		Do()
	if err != nil {
		// Handle error
		m := fmt.Sprintf("Failed to query ES %v", err)
		fmt.Println(m)
		http.Error(w, m, http.StatusInternalServerError)
		return
	}

	fmt.Println("Query took %d milliseconds\n", searchResult.TookInMillis)
	fmt.Printf("Found a total of %d posts\n", searchResult.TotalHits())

	var typ Post
	var ps []Post
	for _, item := range searchResult.Each(reflect.TypeOf(typ)) { //instance of
		p := item.(Post) // p = (Post) item
		fmt.Printf("Post by %s: %s at lat %v and lon %v\n",
			p.User, p.Message, p.Location.Lat, p.Location.Lon)
		fmt.Printf("Find face=%v\n", p.Face)
		ps = append(ps, p)
	}

	js, err := json.Marshal(ps)
	if err != nil {
		m := fmt.Sprintf("Failed to parse post object %v", err)
		fmt.Println(m)
		http.Error(w, m, http.StatusInternalServerError)
		return
	}

	w.Write(js)

}

func handlerSearch(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Received one request for search.")

	lat, _ := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, _ := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)

	ran := DISTANCE
	if val := r.URL.Query().Get("range"); val != "" {
		ran = val + "km"
	}

	fmt.Println("Search received: %f%f%s\n", lat, lon, ran)

	client, err := elastic.NewClient(elastic.SetURL(ES_URL), elastic.SetSniff(false))
	if err != nil {
		panic(err)
	}

	q := elastic.NewGeoDistanceQuery("location")
	q = q.Distance(ran).Lat(lat).Lon(lon)

	searchResult, err := client.Search().
		Index(INDEX).
		Query(q).
		Pretty(true).
		Do()
	if err != nil {
		panic(err)
	}

	fmt.Println("Query took %d milliseconds\n", searchResult.TookInMillis)
	fmt.Printf("Found a total of %d posts\n", searchResult.TotalHits())

	var typ Post
	var ps []Post
	for _, item := range searchResult.Each(reflect.TypeOf(typ)) { //instance of
		p := item.(Post) // p = (Post) item
		fmt.Printf("Post by %s: %s at lat %v and lon %v\n",
			p.User, p.Message, p.Location.Lat, p.Location.Lon)

		ps = append(ps, p)
	}

	js, err := json.Marshal(ps)
	if err != nil {
		panic(err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(js)

}

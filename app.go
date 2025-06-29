package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"html/template"
	"image"
	"image/gif"
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/nfnt/resize"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore

	// テンプレートのキャッシュ
	templates = struct {
		layout *template.Template
		index  *template.Template
		user   *template.Template
		posts  *template.Template
		post   *template.Template
	}{}

	// 画像のキャッシュ
	imageCache = struct {
		sync.RWMutex
		data    map[string]*cacheEntry
		maxSize int64
		curSize int64
	}{
		data:    make(map[string]*cacheEntry),
		maxSize: 100 * 1024 * 1024, // 100MB
	}
)

type cacheEntry struct {
	data    []byte
	lastUse time.Time
}

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
	MaxImageSize  = 800              // 最大画像サイズ
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient := memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// テンプレートの初期化
	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	// レイアウトテンプレート
	templates.layout = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
	))

	// インデックスページ
	templates.index = template.Must(template.New("index.html").Funcs(fmap).ParseFiles(
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	// ユーザーページ
	templates.user = template.Must(template.New("user.html").Funcs(fmap).ParseFiles(
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	// 投稿一覧
	templates.posts = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	// 個別投稿
	templates.post = template.Must(template.New("post.html").Funcs(fmap).ParseFiles(
		getTemplPath("post.html"),
	))
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// 今回のGo実装では言語側のエスケープの仕組みが使えないのでOSコマンドインジェクション対策できない
// 取り急ぎPHPのescapeshellarg関数を参考に自前で実装
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	hasher := sha512.New()
	hasher.Write([]byte(src))
	return hex.EncodeToString(hasher.Sum(nil))
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, err := store.Get(r, "isuconp-go.session")
	if err != nil {
		log.Printf("Failed to get session: %v", err)
		// エラー時は新しいセッションを作成
		session, _ = store.New(r, "isuconp-go.session")
	}
	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}
	err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ? AND `del_flg` = 0", uid)
	if err != nil {
		log.Printf("Failed to get user: %v", err)
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func makePosts(results []Post, csrfToken string, allComments bool) ([]Post, error) {
	var posts []Post
	if len(results) == 0 {
		return posts, nil
	}

	// 投稿IDのリストを作成
	postIDs := make([]int, 0, len(results))
	for _, p := range results {
		postIDs = append(postIDs, p.ID)
	}

	// コメント数とユーザー情報を一括取得
	query := `
		SELECT 
			p.id as post_id,
			COUNT(c.id) as comment_count,
			u.id as user_id,
			u.account_name,
			u.authority,
			u.del_flg,
			u.created_at as user_created_at
		FROM posts p
		LEFT JOIN users u ON p.user_id = u.id
		LEFT JOIN comments c ON p.id = c.post_id
		WHERE p.id IN (?)
		GROUP BY p.id, u.id
	`
	query, args, err := sqlx.In(query, postIDs)
	if err != nil {
		return nil, err
	}

	// 投稿情報をマップに格納
	postMap := make(map[int]*Post)
	for _, p := range results {
		postMap[p.ID] = &p
	}

	rows, err := db.Queryx(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var postID, userID, commentCount int
		var accountName string
		var authority, delFlg int
		var userCreatedAt time.Time

		err := rows.Scan(&postID, &commentCount, &userID, &accountName, &authority, &delFlg, &userCreatedAt)
		if err != nil {
			return nil, err
		}

		if post, ok := postMap[postID]; ok {
			post.CommentCount = commentCount
			post.User = User{
				ID:          userID,
				AccountName: accountName,
				Authority:   authority,
				DelFlg:      delFlg,
				CreatedAt:   userCreatedAt,
			}
		}
	}

	// コメントを一括取得
	commentQuery := `
		SELECT 
			c.*,
			u.id as user_id,
			u.account_name,
			u.authority,
			u.del_flg,
			u.created_at as user_created_at
		FROM comments c
		JOIN users u ON c.user_id = u.id
		WHERE c.post_id IN (?)
	`
	if !allComments {
		commentQuery += ` AND c.id IN (
			SELECT id FROM comments 
			WHERE post_id = c.post_id 
			ORDER BY created_at DESC 
			LIMIT 3
		)`
	}
	commentQuery += ` ORDER BY c.created_at DESC LIMIT ?`

	commentQuery, args, err = sqlx.In(commentQuery, postIDs)
	if err != nil {
		return nil, err
	}

	// コメントの最大数を計算（投稿数 × 3）
	maxComments := len(postIDs) * 3
	args = append(args, maxComments)

	rows, err = db.Queryx(commentQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	commentsMap := make(map[int][]Comment)
	for rows.Next() {
		var comment Comment
		var user User
		err := rows.Scan(
			&comment.ID,
			&comment.PostID,
			&comment.UserID,
			&comment.Comment,
			&comment.CreatedAt,
			&user.ID,
			&user.AccountName,
			&user.Authority,
			&user.DelFlg,
			&user.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		comment.User = user
		commentsMap[comment.PostID] = append(commentsMap[comment.PostID], comment)
	}

	// 結果を組み立てる
	for _, p := range results {
		if post, ok := postMap[p.ID]; ok {
			post.Comments = commentsMap[p.ID]
			post.CSRFToken = csrfToken
			if post.User.DelFlg == 0 {
				posts = append(posts, *post)
			}
		}
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok || csrfToken == nil {
		// CSRFトークンが存在しない場合は新しく生成
		csrfToken = secureRandomStr(16)
		session.Values["csrf_token"] = csrfToken
		session.Save(r, nil)
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	results := []Post{}

	err := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` ORDER BY `created_at` DESC LIMIT ?", postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	templates.layout.ExecuteTemplate(w, "layout.html", struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?", user.ID, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	// ユーザーの統計情報を1つのクエリで取得
	type UserStats struct {
		PostCount      int `db:"post_count"`
		CommentCount   int `db:"comment_count"`
		CommentedCount int `db:"commented_count"`
	}

	var stats UserStats
	err = db.Get(&stats, `
		SELECT 
			(SELECT COUNT(*) FROM posts WHERE user_id = ?) as post_count,
			(SELECT COUNT(*) FROM comments WHERE user_id = ?) as comment_count,
			(SELECT COUNT(DISTINCT post_id) FROM comments WHERE post_id IN (SELECT id FROM posts WHERE user_id = ?)) as commented_count
	`, user.ID, user.ID, user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	me := getSessionUser(r)

	templates.layout.ExecuteTemplate(w, "layout.html", struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, stats.PostCount, stats.CommentCount, stats.CommentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `created_at` <= ? ORDER BY `created_at` DESC", t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	templates.posts.ExecuteTemplate(w, "posts.html", posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	templates.layout.ExecuteTemplate(w, "layout.html", struct {
		Post Post
		Me   User
	}{p, me})
}

// 画像をリサイズする関数
func resizeImage(imgData []byte, mime string) ([]byte, error) {
	// 画像をデコード
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	// 画像をリサイズ
	resized := resize.Resize(MaxImageSize, MaxImageSize, img, resize.Lanczos3)

	// リサイズした画像をエンコード
	var buf bytes.Buffer
	switch mime {
	case "image/jpeg":
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
	case "image/png":
		if err := png.Encode(&buf, resized); err != nil {
			return nil, err
		}
	case "image/gif":
		if err := gif.Encode(&buf, resized, nil); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported image type: %s", mime)
	}

	return buf.Bytes(), nil
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// 画像をリサイズ
	resizedData, err := resizeImage(filedata, mime)
	if err != nil {
		log.Printf("Failed to resize image: %v", err)
		// リサイズに失敗した場合は元の画像を使用
		resizedData = filedata
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		resizedData,
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	// キャッシュをクリア
	clearImageCache()

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

// キャッシュのエントリを追加
func addToCache(key string, data []byte) {
	imageCache.Lock()
	defer imageCache.Unlock()

	// 新しいデータのサイズ
	newSize := int64(len(data))

	// キャッシュが一杯の場合、古いエントリを削除
	for imageCache.curSize+newSize > imageCache.maxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range imageCache.data {
			if oldestKey == "" || v.lastUse.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastUse
			}
		}
		if oldestKey != "" {
			imageCache.curSize -= int64(len(imageCache.data[oldestKey].data))
			delete(imageCache.data, oldestKey)
		} else {
			break
		}
	}

	// 新しいエントリを追加
	imageCache.data[key] = &cacheEntry{
		data:    data,
		lastUse: time.Now(),
	}
	imageCache.curSize += newSize
}

// キャッシュからエントリを取得
func getFromCache(key string) ([]byte, bool) {
	imageCache.RLock()
	entry, found := imageCache.data[key]
	imageCache.RUnlock()

	if !found {
		return nil, false
	}

	// 最終使用時間を更新
	imageCache.Lock()
	entry.lastUse = time.Now()
	imageCache.Unlock()

	return entry.data, true
}

func getImage(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	ext := r.PathValue("ext")
	cacheKey := fmt.Sprintf("%d.%s", pid, ext)

	// キャッシュから画像を取得
	imgdata, found := getFromCache(cacheKey)

	if !found {
		// キャッシュにない場合はDBから取得
		post := Post{}
		err := db.Get(&post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
		if err != nil {
			log.Print(err)
			return
		}

		if ext == "jpg" && post.Mime == "image/jpeg" ||
			ext == "png" && post.Mime == "image/png" ||
			ext == "gif" && post.Mime == "image/gif" {
			imgdata = post.Imgdata

			// キャッシュに保存
			addToCache(cacheKey, imgdata)
		} else {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	// キャッシュヘッダーを設定
	w.Header().Set("Content-Type", getMimeType(ext))
	w.Header().Set("Cache-Control", "public, max-age=31536000") // 1年間キャッシュ
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(imgdata)))

	// If-None-Matchヘッダーをチェック
	if match := r.Header.Get("If-None-Match"); match != "" {
		if match == fmt.Sprintf(`"%x"`, sha256.Sum256(imgdata)) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	_, err = w.Write(imgdata)
	if err != nil {
		log.Print(err)
		return
	}
}

func getMimeType(ext string) string {
	switch ext {
	case "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	default:
		return ""
	}
}

// キャッシュをクリアする関数
func clearImageCache() {
	imageCache.Lock()
	imageCache.data = make(map[string]*cacheEntry)
	imageCache.curSize = 0
	imageCache.Unlock()
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[a-zA-Z]+}`, getAccountName)
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.FileServer(http.Dir("../public")).ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(":8080", r))
}

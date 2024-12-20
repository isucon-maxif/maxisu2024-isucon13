package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSessionIDKey      = "SESSIONID"
	defaultSessionExpiresKey = "EXPIRES"
	defaultUserIDKey         = "USERID"
	defaultUsernameKey       = "USERNAME"
	bcryptDefaultCost        = bcrypt.MinCost
)

var fallbackImage = "../img/NoImage.jpg"

type UserModel struct {
	ID             int64  `db:"id"`
	Name           string `db:"name"`
	DisplayName    string `db:"display_name"`
	Description    string `db:"description"`
	HashedPassword string `db:"password"`
}

type User struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Theme       Theme  `json:"theme,omitempty"`
	IconHash    string `json:"icon_hash,omitempty"`
}

type Theme struct {
	ID       int64 `json:"id"`
	DarkMode bool  `json:"dark_mode"`
}

type ThemeModel struct {
	ID       int64 `db:"id"`
	UserID   int64 `db:"user_id"`
	DarkMode bool  `db:"dark_mode"`
}

type PostUserRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Password is non-hashed password.
	Password string               `json:"password"`
	Theme    PostUserRequestTheme `json:"theme"`
}

type PostUserRequestTheme struct {
	DarkMode bool `json:"dark_mode"`
}

type LoginRequest struct {
	Username string `json:"username"`
	// Password is non-hashed password.
	Password string `json:"password"`
}

type PostIconRequest struct {
	Image []byte `json:"image"`
}

type PostIconResponse struct {
	ID int64 `json:"id"`
}

func getIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	username := c.Param("username")

	ifNoneMatch := c.Request().Header.Get("If-None-Match")

	if ifNoneMatch != "" {
		trimmedIfNoneMatch := ifNoneMatch[1 : len(ifNoneMatch)-1]
		IconHashByUsernameCacheMutex.RLock()
		if hash, ok := IconHashByUsernameCache[username]; ok && hash == trimmedIfNoneMatch {
			IconHashByUsernameCacheMutex.RUnlock()
			return c.NoContent(http.StatusNotModified)
		}
		IconHashByUsernameCacheMutex.RUnlock()
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var user UserModel
	if err := tx.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	var image []byte
	if err := tx.GetContext(ctx, &image, "SELECT image FROM icons WHERE user_id = ?", user.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.File(fallbackImage)
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user icon: "+err.Error())
		}
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(image))
	IconHashByUsernameCacheMutex.Lock()
	IconHashByUsernameCache[username] = hash
	IconHashByUsernameCacheMutex.Unlock()
	IconHashByUserIDCacheMutex.Lock()
	IconHashByUserIDCache[user.ID] = hash
	IconHashByUserIDCacheMutex.Unlock()

	return c.Blob(http.StatusOK, "image/jpeg", image)
}

func postIconHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var req *PostIconRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM icons WHERE user_id = ?", userID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to delete old user icon: "+err.Error())
	}

	rs, err := tx.ExecContext(ctx, "INSERT INTO icons (user_id, image) VALUES (?, ?)", userID, req.Image)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert new user icon: "+err.Error())
	}

	iconID, err := rs.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted icon id: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	UserByIDCacheMutex.Lock()
	delete(UserByIDCache, userID)
	UserByIDCacheMutex.Unlock()
	deleteLivestreamByIDCacheByOwnerID(userID)
	deleteLivecommentByIDCacheByOwnerID(userID)

	return c.JSON(http.StatusCreated, &PostIconResponse{
		ID: iconID,
	})
}

func getMeHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{}
	err = tx.GetContext(ctx, &userModel, "SELECT * FROM users WHERE id = ?", userID)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "not found user that has the userid in session")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponse(ctx, tx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

// ユーザ登録API
// POST /api/register
func registerHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := PostUserRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	if req.Name == "pipe" {
		return echo.NewHTTPError(http.StatusBadRequest, "the username 'pipe' is reserved")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptDefaultCost)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to generate hashed password: "+err.Error())
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{
		Name:           req.Name,
		DisplayName:    req.DisplayName,
		Description:    req.Description,
		HashedPassword: string(hashedPassword),
	}

	result, err := tx.NamedExecContext(ctx, "INSERT INTO users (name, display_name, description, password) VALUES(:name, :display_name, :description, :password)", userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user: "+err.Error())
	}

	userID, err := result.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted user id: "+err.Error())
	}

	userModel.ID = userID

	themeModel := ThemeModel{
		UserID:   userID,
		DarkMode: req.Theme.DarkMode,
	}
	if _, err := tx.NamedExecContext(ctx, "INSERT INTO themes (user_id, dark_mode) VALUES(:user_id, :dark_mode)", themeModel); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert user theme: "+err.Error())
	}

	// post request to powerdns
	{
		endpoint := "http://192.168.0.4:8081/api/v1/servers/localhost/zones/u.isucon.local."
		body := fmt.Sprintf(`{"rrsets": [{"name": "%s.u.isucon.local.", "type": "A", "ttl": 3600, "changetype": "REPLACE", "records": [{"content": "%s", "disabled": false}]}]}`, req.Name, powerDNSSubdomainAddress)
		req, err := http.NewRequest(http.MethodPatch, endpoint, strings.NewReader(body))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to create request to powerdns: "+err.Error())
		}
		req.Header.Set("X-API-Key", "isudns")
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to request to powerdns: "+err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to request to powerdns: status code is not 204")
		}
	}
	// if out, err := exec.Command("pdnsutil", "add-record", "u.isucon.dev", req.Name, "A", "3600", powerDNSSubdomainAddress).CombinedOutput(); err != nil {
	// 	return echo.NewHTTPError(http.StatusInternalServerError, string(out)+": "+err.Error())
	// }

	user, err := fillUserResponse(ctx, tx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	UserByIDCacheMutex.Lock()
	delete(UserByIDCache, userID)
	UserByIDCacheMutex.Unlock()
	deleteLivestreamByIDCacheByOwnerID(userID)
	deleteLivecommentByIDCacheByOwnerID(userID)

	return c.JSON(http.StatusCreated, user)
}

// ユーザログインAPI
// POST /api/login
func loginHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	req := LoginRequest{}
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{}
	// usernameはUNIQUEなので、whereで一意に特定できる
	err = tx.GetContext(ctx, &userModel, "SELECT * FROM users WHERE name = ?", req.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	err = bcrypt.CompareHashAndPassword([]byte(userModel.HashedPassword), []byte(req.Password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid username or password")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to compare hash and password: "+err.Error())
	}

	sessionEndAt := time.Now().Add(1 * time.Hour)

	sessionID := uuid.NewString()

	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sess.Options = &sessions.Options{
		Domain: "u.isucon.local",
		MaxAge: int(60000),
		Path:   "/",
	}
	sess.Values[defaultSessionIDKey] = sessionID
	sess.Values[defaultUserIDKey] = userModel.ID
	sess.Values[defaultUsernameKey] = userModel.Name
	sess.Values[defaultSessionExpiresKey] = sessionEndAt.Unix()

	if err := sess.Save(c.Request(), c.Response()); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to save session: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// ユーザ詳細API
// GET /api/user/:username
func getUserHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	username := c.Param("username")

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	userModel := UserModel{}
	if err := tx.GetContext(ctx, &userModel, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
	}

	user, err := fillUserResponse(ctx, tx, userModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill user: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, user)
}

func verifyUserSession(c echo.Context) error {
	sess, err := session.Get(defaultSessionIDKey, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get session")
	}

	sessionExpires, ok := sess.Values[defaultSessionExpiresKey]
	if !ok {
		return echo.NewHTTPError(http.StatusForbidden, "failed to get EXPIRES value from session")
	}

	_, ok = sess.Values[defaultUserIDKey].(int64)
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, "failed to get USERID value from session")
	}

	now := time.Now()
	if now.Unix() > sessionExpires.(int64) {
		return echo.NewHTTPError(http.StatusUnauthorized, "session has expired")
	}

	return nil
}

func fillUserResponse(ctx context.Context, tx *sqlx.Tx, userModel UserModel) (User, error) {
	UserByIDCacheMutex.RLock()
	if user, ok := UserByIDCache[userModel.ID]; ok {
		UserByIDCacheMutex.RUnlock()
		return user, nil
	}
	UserByIDCacheMutex.RUnlock()

	themeModel := ThemeModel{}
	if err := tx.GetContext(ctx, &themeModel, "SELECT * FROM themes WHERE user_id = ?", userModel.ID); err != nil {
		return User{}, err
	}

	IconHashByUserIDCacheMutex.RLock()
	hashStr, ok := IconHashByUserIDCache[userModel.ID]
	IconHashByUserIDCacheMutex.RUnlock()

	var image []byte
	isFallbackImage := false
	if !ok {
		if err := tx.GetContext(ctx, &image, "SELECT image FROM icons WHERE user_id = ?", userModel.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return User{}, err
			}
			image, err = os.ReadFile(fallbackImage)
			if err != nil {
				return User{}, err
			}
			isFallbackImage = true
		}
		hashStr = fmt.Sprintf("%x", sha256.Sum256(image))
	}

	if !isFallbackImage {
		IconHashByUserIDCacheMutex.Lock()
		IconHashByUserIDCache[userModel.ID] = hashStr
		IconHashByUserIDCacheMutex.Unlock()
	}

	user := User{
		ID:          userModel.ID,
		Name:        userModel.Name,
		DisplayName: userModel.DisplayName,
		Description: userModel.Description,
		Theme: Theme{
			ID:       themeModel.ID,
			DarkMode: themeModel.DarkMode,
		},
		IconHash: hashStr,
	}

	UserByIDCacheMutex.Lock()
	UserByIDCache[userModel.ID] = user
	UserByIDCacheMutex.Unlock()

	return user, nil
}

// N+1問題を解消するためにbulkで取得する
func fillUserResponseBulk(ctx context.Context, tx *sqlx.Tx, userModels []*UserModel) ([]User, error) {
	// if len(userModels) == 0 {
	// 	return []User{}, nil
	// }
	// cachedUsers := make([]User, 0, len(userModels))
	// uncachedUserModels := make([]*UserModel, 0, len(userModels))

	// UserByIDCacheMutex.RLock()
	// for _, userModel := range userModels {
	// 	if user, ok := UserByIDCache[userModel.ID]; ok {
	// 		cachedUsers = append(cachedUsers, user)
	// 	} else {
	// 		uncachedUserModels = append(uncachedUserModels, userModel)
	// 	}
	// }
	// UserByIDCacheMutex.RUnlock()

	// if len(uncachedUserModels) == 0 {
	// 	return cachedUsers, nil
	// }

	// // user_idのリストを作成
	// userIDs := make([]int64, len(uncachedUserModels))
	// for i, userModel := range uncachedUserModels {
	// 	userIDs[i] = userModel.ID
	// }

	// // themeを取得
	// themeModels := make([]ThemeModel, 0, len(uncachedUserModels))
	// query, args, err := sqlx.In("SELECT * FROM themes WHERE user_id IN (?)", userIDs)
	// if err != nil {
	// 	return nil, err
	// }
	// query = tx.Rebind(query)
	// if err := tx.SelectContext(ctx, &themeModels, query, args...); err != nil {
	// 	return nil, err
	// }

	// // キャッシュされていないuser_idのリストを作成
	// uncachedUserIDs := make([]int64, 0, len(uncachedUserModels))
	// iconHashStringMap := make(map[int64]string, len(uncachedUserModels))

	// IconHashByUserIDCacheMutex.RLock()
	// for _, userModel := range uncachedUserModels {
	// 	hash, ok := IconHashByUserIDCache[userModel.ID]
	// 	if !ok {
	// 		uncachedUserIDs = append(uncachedUserIDs, userModel.ID)
	// 	} else {
	// 		iconHashStringMap[userModel.ID] = hash
	// 	}
	// }
	// IconHashByUserIDCacheMutex.RUnlock()

	// // キャッシュされていないアイコンを取得
	// if len(uncachedUserIDs) > 0 {
	// 	icons := make([]struct {
	// 		UserID int64  `db:"user_id"`
	// 		Image  []byte `db:"image"`
	// 	}, 0, len(uncachedUserIDs))
	// 	query, args, err = sqlx.In("SELECT user_id, image FROM icons WHERE user_id IN (?)", uncachedUserIDs)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	query = tx.Rebind(query)
	// 	if err := tx.SelectContext(ctx, &icons, query, args...); err != nil {
	// 		return nil, err
	// 	}

	// 	IconHashByUserIDCacheMutex.Lock()
	// 	for _, icon := range icons {
	// 		hash := fmt.Sprintf("%x", sha256.Sum256(icon.Image))
	// 		iconHashStringMap[icon.UserID] = hash
	// 		IconHashByUserIDCache[icon.UserID] = hash
	// 	}
	// 	IconHashByUserIDCacheMutex.Unlock()
	// }

	// users := []User{}
	// for i, userModel := range uncachedUserModels {
	// 	iconHash, ok := iconHashStringMap[userModel.ID]
	// 	if !ok {
	// 		icon, err := os.ReadFile(fallbackImage)
	// 		if err != nil {
	// 			return nil, err
	// 		}
	// 		iconHash = fmt.Sprintf("%x", sha256.Sum256(icon))
	// 	}

	// 	user := User{
	// 		ID:          userModel.ID,
	// 		Name:        userModel.Name,
	// 		DisplayName: userModel.DisplayName,
	// 		Description: userModel.Description,
	// 		Theme: Theme{
	// 			ID:       themeModels[i].ID,
	// 			DarkMode: themeModels[i].DarkMode,
	// 		},
	// 		IconHash: iconHash,
	// 	}
	// 	users = append(users, user)
	// }

	// UserByIDCacheMutex.Lock()
	// for i, user := range users {
	// 	UserByIDCache[userModels[i].ID] = user
	// }
	// UserByIDCacheMutex.Unlock()

	// users = append(cachedUsers, users...)

	// return users, nil

	users := make([]User, len(userModels))
	for i, userModel := range userModels {
		user, err := fillUserResponse(ctx, tx, *userModel)
		if err != nil {
			return nil, err
		}
		users[i] = user
	}

	return users, nil
}

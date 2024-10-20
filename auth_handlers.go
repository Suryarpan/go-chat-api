package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Suryarpan/chat-api/internal/apiconf"
	"github.com/Suryarpan/chat-api/internal/auth"
	"github.com/Suryarpan/chat-api/internal/database"
	"github.com/Suryarpan/chat-api/render"
	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/pbkdf2"
)

const (
	internalServerErrorMssg      = "could not process request at this time"
	insufficientStorageErrorMssg = "could not create user at this moment"
	tokenGenerationErrorMssg     = "could not login user at this time"
)

func saltyPassword(password, salt []byte) []byte {
	iterations := 10_000
	hashed := pbkdf2.Key(password, salt, iterations, 512, sha256.New)
	return hashed
}

type loginUserData struct {
	Username string `json:"username" validate:"required,min=5,max=50"`
	Password string `json:"password" validate:"required,printascii,min=8"`
}

type loginResponse struct {
	Token        string           `json:"token"`
	TokenType    string           `json:"token_type"`
	Username     string           `json:"username"`
	DisplayName  string           `json:"display_name"`
	LastLoggedIn pgtype.Timestamp `json:"last_logged_in"`
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	lu := loginUserData{}
	reader := json.NewDecoder(r.Body)
	reader.Decode(&lu)

	apiCfg := apiconf.GetConfig(r)

	err := apiCfg.Validate.Struct(lu)

	if err != nil {
		validationError, ok := err.(validator.ValidationErrors)
		if !ok {
			slog.Error("error with validator definition", "error", err)
			render.RespondFailure(w, http.StatusInternalServerError, internalServerErrorMssg)
		} else {
			render.RespondValidationFailure(w, validationError)
		}
		return
	}

	queries := database.New(apiCfg.ConnPool)
	user, err := queries.GetUserByName(r.Context(), lu.Username)
	if err != nil {
		render.RespondFailure(w, http.StatusBadRequest, "username or password is invalid")
		return
	}
	hashedPassword := saltyPassword([]byte(lu.Password), user.PasswordSalt)
	if subtle.ConstantTimeCompare(hashedPassword, user.Password) != 1 {
		render.RespondFailure(w, http.StatusBadRequest, "username or password is invalid")
		return
	}

	err = queries.UpdateLoggedInTime(r.Context(), database.UpdateLoggedInTimeParams{
		LastLoggedIn: pgtype.Timestamp{
			Time:  time.Now().UTC(),
			Valid: true,
		},
		PvtID: user.PvtID,
	})
	if err != nil {
		render.RespondFailure(w, http.StatusInsufficientStorage, tokenGenerationErrorMssg)
	}

	token, err := auth.UserToToken(user)
	if err != nil {
		render.RespondFailure(w, http.StatusInternalServerError, tokenGenerationErrorMssg)
		return
	}
	render.RespondSuccess(w, 200, loginResponse{
		Token:        token,
		TokenType:    auth.TokenPrefix,
		Username:     user.Username,
		DisplayName:  user.DisplayName,
		LastLoggedIn: user.LastLoggedIn,
	})
}

type registerUserData struct {
	Username        string `json:"username" validate:"required,min=5,max=50"`
	DisplayName     string `json:"display_name" validate:"required,min=5,max=150"`
	Password        string `json:"password" validate:"required,printascii,min=8,eqfield=ConfirmPassword"`
	ConfirmPassword string `json:"confirm_password" validate:"required"`
}

type registerResponse struct {
	UserId      pgtype.UUID `json:"user_id"`
	Username    string      `json:"username"`
	DisplayName string      `json:"display_name"`
	CreatedAt   time.Time   `json:"created_at"`
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	ru := registerUserData{}
	reader := json.NewDecoder(r.Body)
	reader.Decode(&ru)

	apiCfg := apiconf.GetConfig(r)
	// validate incoming data
	err := apiCfg.Validate.Struct(ru)
	if err != nil {
		validationErrors, ok := err.(validator.ValidationErrors)
		if !ok {
			slog.Error("error with validator definition", "error", err)
			render.RespondFailure(w, http.StatusInternalServerError, internalServerErrorMssg)
		} else {
			render.RespondValidationFailure(w, validationErrors)
		}
		return
	}
	// check user name with DB
	queries := database.New(apiCfg.ConnPool)
	_, err = queries.GetUserByName(r.Context(), ru.Username)
	if err == nil {
		render.RespondFailure(w, http.StatusNotAcceptable, map[string]string{"username": "already exists"})
		return
	}
	// generate the password hash
	passwordSalt := make([]byte, 128)
	_, err = rand.Read(passwordSalt)
	if err != nil {
		render.RespondFailure(w, http.StatusInsufficientStorage, insufficientStorageErrorMssg)
		return
	}
	password := saltyPassword([]byte(ru.Password), passwordSalt)
	// store in DB
	user, err := queries.CreateUser(r.Context(), database.CreateUserParams{
		Username:     ru.Username,
		DisplayName:  ru.DisplayName,
		Password:     password,
		PasswordSalt: passwordSalt,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			slog.Error(
				"could not create user",
				"error", pgErr.Message,
				"code", pgErr.Code,
				"constraint", pgErr.ConstraintName,
			)
		}
		render.RespondFailure(w, http.StatusInsufficientStorage, insufficientStorageErrorMssg)
		return
	}
	// send back user data
	render.RespondSuccess(w, http.StatusCreated, registerResponse{
		UserId:      user.UserID,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		CreatedAt:   user.CreatedAt,
	})
}

func AuthRouter() *chi.Mux {
	authRouter := chi.NewRouter()

	authRouter.Post("/login", handleLogin)
	authRouter.Post("/register", handleRegister)

	return authRouter
}
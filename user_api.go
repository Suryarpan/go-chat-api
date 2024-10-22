package main

import (
	"crypto/rand"
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
)

const (
	insufficientStorageErrorMssg = "could not create user at this moment"
)

type PublicUserDetails struct {
	UserID       pgtype.UUID      `json:"user_id"`
	Username     string           `json:"username"`
	DisplayName  string           `json:"display_name"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at,omitempty"`
	LastLoggedIn pgtype.Timestamp `json:"last_logged_in,omitempty"`
}

func convertToPublicUser(u database.User) PublicUserDetails {
	return PublicUserDetails{
		UserID:       u.UserID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
		LastLoggedIn: u.LastLoggedIn,
	}
}

func handleGetUserDetail(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUserData(r)

	publicData := convertToPublicUser(user)
	render.RespondSuccess(w, http.StatusOK, publicData)
}

type createUserData struct {
	Username    string `json:"username" validate:"required,min=5,max=50"`
	DisplayName string `json:"display_name" validate:"required,min=5,max=150"`
	Password    string `json:"password" validate:"required,printascii,min=8"`
}

func handleCreateUser(w http.ResponseWriter, r *http.Request) {
	cu := createUserData{}
	reader := json.NewDecoder(r.Body)
	reader.Decode(&cu)

	apiCfg := apiconf.GetConfig(r)
	// validate incoming data
	err := apiCfg.Validate.Struct(cu)
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
	_, err = queries.GetUserByName(r.Context(), cu.Username)
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
	password := auth.SaltyPassword([]byte(cu.Password), passwordSalt)
	// store in DB
	user, err := queries.CreateUser(r.Context(), database.CreateUserParams{
		Username:     cu.Username,
		DisplayName:  cu.DisplayName,
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
	render.RespondSuccess(w, http.StatusCreated, PublicUserDetails{
		UserID:      user.UserID,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		CreatedAt:   user.CreatedAt,
	})
}

type updateUserData struct {
	Username    *string `json:"username" validate:"omitnil,min=5,max=50"`
	DisplayName *string `json:"display_name" validate:"omitnil,min=5,max=150"`
	Password    *string `json:"password" validate:"omitnil,printascii,min=8"`
}

func handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	ud := updateUserData{}
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&ud)
	user := auth.GetUserData(r)
	// nothing to update
	emptyData := updateUserData{}
	if ud == emptyData {
		render.RespondSuccess(w, http.StatusOK, convertToPublicUser(user))
		return
	}
	apiCfg := apiconf.GetConfig(r)
	// validate incoming data
	err := apiCfg.Validate.Struct(ud)
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
	// find the updated fields
	if ud.Password != nil {
		updatedPassword := auth.SaltyPassword([]byte(*ud.Password), user.PasswordSalt)
		user.Password = updatedPassword
	}
	if ud.Username != nil {
		user.Username = *ud.Username
	}
	if ud.DisplayName != nil {
		user.DisplayName = *ud.DisplayName
	}
	// update in DB
	queries := database.New(apiCfg.ConnPool)
	updUser, err := queries.UpdateUserDetails(r.Context(), database.UpdateUserDetailsParams{
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Password:    user.Password,
		UpdatedAt:   time.Now().UTC(),
		PvtID:       user.PvtID,
	})
	if err != nil {
		render.RespondFailure(w, http.StatusInsufficientStorage, "could not update at this time")
		return
	}
	render.RespondSuccess(w, http.StatusOK, convertToPublicUser(updUser))
}

func handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUserData(r)
	apiCfg := apiconf.GetConfig(r)
	queries := database.New(apiCfg.ConnPool)
	delUser, err := queries.DeleteUserDetails(r.Context(), user.PvtID)
	if err != nil {
		render.RespondFailure(w, http.StatusInternalServerError, "could not delete at this time")
		return
	}
	render.RespondSuccess(w, http.StatusOK, delUser)
}

func UserRouter() *chi.Mux {
	router := chi.NewMux()

	router.With(auth.Authentication).Group(func(r chi.Router) {
		r.Get("/", handleGetUserDetail)
		r.Patch("/", handleUpdateUser)
		r.Delete("/", handleDeleteUser)
	})
	router.Post("/", handleCreateUser)

	return router
}

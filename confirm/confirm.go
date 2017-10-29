// Package confirm implements confirmation of user registration via e-mail
package confirm

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"path"

	"github.com/volatiletech/authboss"
	"github.com/volatiletech/authboss/internal/response"
	"strings"
	"time"
)

// Storer and FormValue constants
const (
	StoreConfirmToken = "confirm_token"
	StoreConfirmed    = "confirmed"

	FormValueConfirm = "cnf"

	tplConfirmHTML = "confirm_email.html.tpl"
	tplConfirmText = "confirm_email.txt.tpl"

	// for randomize confirm token (length 6)
	TokenLength   = 6
	letterBytes   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

var (
	errUserMissing = errors.New("confirm: After registration user must be loaded")
)

// ConfirmStorer must be implemented in order to satisfy the confirm module's
// storage requirements.
type ConfirmStorer interface {
	authboss.Storer
	// ConfirmUser looks up a user by a confirm token. See confirm module for
	// attribute names. If the token is not found in the data store,
	// simply return nil, ErrUserNotFound.
	ConfirmUser(confirmToken string) (interface{}, error)
}

func init() {
	authboss.RegisterModule("confirm", &Confirm{})
	rand.Seed(time.Now().UnixNano())
}

// generate random string
func RandStringBytesMaskImpr(n int) string {
	b := make([]byte, n)
	// A rand.Int63() generates 63 random bits, enough for letterIdxMax letters!
	for i, cache, remain := n-1, rand.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = rand.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return string(b)
}

// Confirm module
type Confirm struct {
	*authboss.Authboss
	emailHTMLTemplates response.Templates
	emailTextTemplates response.Templates
}

// Initialize the module
func (c *Confirm) Initialize(ab *authboss.Authboss) (err error) {
	c.Authboss = ab

	var ok bool
	storer, ok := c.Storer.(ConfirmStorer)
	if c.StoreMaker == nil && (storer == nil || !ok) {
		return errors.New("confirm: Need a ConfirmStorer")
	}

	c.emailHTMLTemplates, err = response.LoadTemplates(ab, c.LayoutHTMLEmail, c.ViewsPath, tplConfirmHTML)
	if err != nil {
		return err
	}
	c.emailTextTemplates, err = response.LoadTemplates(ab, c.LayoutTextEmail, c.ViewsPath, tplConfirmText)
	if err != nil {
		return err
	}

	c.Callbacks.After(authboss.EventGetUser, func(ctx *authboss.Context) error {
		_, err := c.beforeGet(ctx)
		return err
	})
	c.Callbacks.Before(authboss.EventAuth, c.beforeGet)
	c.Callbacks.After(authboss.EventRegister, c.afterRegister)

	return nil
}

// Routes for the module
func (c *Confirm) Routes() authboss.RouteTable {
	return authboss.RouteTable{
		"/confirm": c.confirmHandler,
	}
}

// Storage requirements
func (c *Confirm) Storage() authboss.StorageOptions {
	return authboss.StorageOptions{
		c.PrimaryID:         authboss.String,
		authboss.StoreEmail: authboss.String,
		StoreConfirmToken:   authboss.String,
		StoreConfirmed:      authboss.Bool,
	}
}

func (c *Confirm) beforeGet(ctx *authboss.Context) (authboss.Interrupt, error) {
	if confirmed, err := ctx.User.BoolErr(StoreConfirmed); err != nil {
		return authboss.InterruptNone, err
	} else if !confirmed {
		return authboss.InterruptAccountNotConfirmed, nil
	}

	return authboss.InterruptNone, nil
}

// AfterRegister ensures the account is not activated.
func (c *Confirm) afterRegister(ctx *authboss.Context) error {
	if ctx.User == nil {
		return errUserMissing
	}

	// changes to generate 6-characters token
	token := RandStringBytesMaskImpr(TokenLength)

	ctx.User[StoreConfirmToken] = strings.ToUpper(token)

	if err := ctx.SaveUser(); err != nil {
		return err
	}

	email, err := ctx.User.StringErr(authboss.StoreEmail)
	if err != nil {
		return err
	}

	goConfirmEmail(c, ctx, email, token)

	return nil
}

var goConfirmEmail = func(c *Confirm, ctx *authboss.Context, to, token string) {
	if ctx.MailMaker != nil {
		c.confirmEmail(ctx, to, token)
	} else {
		go c.confirmEmail(ctx, to, token)
	}
}

// confirmEmail sends a confirmation e-mail.
func (c *Confirm) confirmEmail(ctx *authboss.Context, to, token string) {
	p := path.Join(c.MountPath, "confirm")
	url := fmt.Sprintf("%s%s?%s=%s", c.RootURL, p, url.QueryEscape(FormValueConfirm), url.QueryEscape(token))

	email := authboss.Email{
		To:      []string{to},
		From:    c.EmailFrom,
		Subject: c.EmailSubjectPrefix + "Confirm New Account",
	}

	err := response.Email(ctx.Mailer, email, c.emailHTMLTemplates, tplConfirmHTML, c.emailTextTemplates, tplConfirmText, url)
	if err != nil {
		fmt.Fprintf(ctx.LogWriter, "confirm: Failed to send e-mail: %v", err)
	}
}

func (c *Confirm) confirmHandler(ctx *authboss.Context, w http.ResponseWriter, r *http.Request) error {
	token := r.FormValue(FormValueConfirm)
	if len(token) == 0 {
		return authboss.ClientDataErr{Name: FormValueConfirm}
	}

	user, err := ctx.Storer.(ConfirmStorer).ConfirmUser(strings.ToUpper(token))
	if err == authboss.ErrUserNotFound {
		return authboss.ErrAndRedirect{Location: "/", Err: errors.New("confirm: token not found")}
	} else if err != nil {
		return err
	}

	ctx.User = authboss.Unbind(user)

	ctx.User[StoreConfirmToken] = ""
	ctx.User[StoreConfirmed] = true

	if err := ctx.SaveUser(); err != nil {
		return err
	}
	if c.Authboss.AllowInsecureLoginAfterConfirm {
		key, err := ctx.User.StringErr(c.PrimaryID)
		if err != nil {
			return err
		}
		ctx.SessionStorer.Put(authboss.SessionKey, key)
	}
	response.Redirect(ctx, w, r, c.RegisterOKPath, "You have successfully confirmed your account.", "", true)

	return nil
}

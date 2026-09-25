package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore/pgtest"
)

var ctx = context.Background()

func newUser(t *testing.T, db *pgstore.DB) pgstore.User {
	t.Helper()
	u, err := db.CreateUser(ctx, pgtest.Email("user"), "hash")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newShop(t *testing.T, db *pgstore.DB, userID int64, clientID string) pgstore.Shop {
	t.Helper()
	s, err := db.AddShop(ctx, pgstore.Shop{
		UserID: userID, Name: "Магазин " + clientID, OzonClientID: clientID,
		APIKeyEnc: []byte{1, 2, 3, 0, 255}, APIKeyHint: "…abcd",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := pgtest.Open(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("повторная миграция: %v", err)
	}
}

func TestEmailIsCaseInsensitiveAndUnique(t *testing.T) {
	db := pgtest.Open(t)
	email := pgtest.Email("Case")
	u, err := db.CreateUser(ctx, strings.ToUpper(email), "h")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != strings.ToLower(email) {
		t.Errorf("адрес не нормализован: %q", u.Email)
	}
	if _, err := db.CreateUser(ctx, email, "h"); !errors.Is(err, pgstore.ErrEmailTaken) {
		t.Fatalf("повторная регистрация: %v", err)
	}
	got, err := db.UserByEmail(ctx, "  "+strings.ToUpper(email)+" ")
	if err != nil || got.ID != u.ID {
		t.Fatalf("поиск без учёта регистра: %v %+v", err, got)
	}
}

func TestShopsAreIsolatedBetweenUsers(t *testing.T) {
	db := pgtest.Open(t)
	alice, bob := newUser(t, db), newUser(t, db)
	s := newShop(t, db, alice.ID, "111")

	if _, err := db.Shop(ctx, bob.ID, s.ID); !errors.Is(err, pgstore.ErrNotFound) {
		t.Fatalf("чужой магазин виден: %v", err)
	}
	if err := db.DeleteShop(ctx, bob.ID, s.ID); !errors.Is(err, pgstore.ErrNotFound) {
		t.Fatalf("чужой магазин удалился: %v", err)
	}
	got, err := db.Shop(ctx, alice.ID, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.APIKeyEnc) != string([]byte{1, 2, 3, 0, 255}) {
		t.Errorf("шифротекст исказился: %v", got.APIKeyEnc)
	}
	// Тот же Client-Id у другого пользователя — законно (например,
	// сотрудник и владелец).
	newShop(t, db, bob.ID, "111")
	if _, err := db.AddShop(ctx, pgstore.Shop{UserID: alice.ID, Name: "x", OzonClientID: "111",
		APIKeyEnc: []byte{1}, APIKeyHint: "h"}); !errors.Is(err, pgstore.ErrShopExists) {
		t.Fatalf("дубль магазина: %v", err)
	}
}

func issue(t *testing.T, st oauth.Storage, userID, shopID int64, hash, grant string, withRefresh bool, clientID ...string) {
	t.Helper()
	now := time.Now()
	cid := "c1"
	if len(clientID) > 0 {
		cid = clientID[0]
	}
	at := &oauth.Token{TokenHash: hash, ClientID: cid, Scopes: []string{oauth.ScopeRead},
		Resource: "https://x/mcp", IssuedAt: now, ExpiresAt: now.Add(time.Hour), GrantID: grant,
		Subject: oauth.Subject{UserID: userID, ShopID: shopID}}
	var rt *oauth.RefreshToken
	if withRefresh {
		rt = &oauth.RefreshToken{TokenHash: "r-" + hash, ClientID: cid, Scopes: at.Scopes,
			Resource: at.Resource, ExpiresAt: now.Add(24 * time.Hour), GrantID: grant, Subject: at.Subject}
	}
	if err := st.SaveTokens(ctx, at, rt); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthStorageRoundTrip(t *testing.T) {
	db := pgtest.Open(t)
	st := db.OAuth()
	u := newUser(t, db)
	s := newShop(t, db, u.ID, "222")

	client := &oauth.Client{ID: pgtest.Email("client"), Name: "Claude",
		RedirectURIs: []string{"https://claude.ai/cb", "http://localhost:1/cb"}, CreatedAt: time.Now()}
	if err := st.SaveClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	got, err := st.Client(ctx, client.ID)
	if err != nil || len(got.RedirectURIs) != 2 || got.RedirectURIs[1] != "http://localhost:1/cb" {
		t.Fatalf("клиент: %v %+v", err, got)
	}
	if _, err := st.Client(ctx, "нет-такого"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("неизвестный клиент: %v", err)
	}

	sess := &oauth.LoginSession{ID: "sess-" + client.ID, ClientID: client.ID, RedirectURI: "https://claude.ai/cb",
		State: "st", Scopes: []string{"ozon:read", "offline_access"}, Resource: "https://x/mcp",
		CodeChallenge: "ch", UserID: u.ID, ExpiresAt: time.Now().Add(time.Minute)}
	if err := st.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Session(ctx, sess.ID); err != nil || got.UserID != u.ID || len(got.Scopes) != 2 {
		t.Fatalf("сессия: %v %+v", err, got)
	}
	if _, err := st.TakeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeSession(ctx, sess.ID); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatal("сессия согласия должна быть одноразовой")
	}

	code := &oauth.AuthCode{CodeHash: "code-" + client.ID, ClientID: client.ID, RedirectURI: "https://claude.ai/cb",
		Scopes: []string{"ozon:read"}, Resource: "https://x/mcp", CodeChallenge: "ch",
		ExpiresAt: time.Now().Add(time.Minute), Subject: oauth.Subject{UserID: u.ID, ShopID: s.ID}}
	if err := st.SaveCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	gotCode, err := st.TakeCode(ctx, code.CodeHash)
	if err != nil || gotCode.ShopID != s.ID || gotCode.UserID != u.ID {
		t.Fatalf("код: %v %+v", err, gotCode)
	}
	if _, err := st.TakeCode(ctx, code.CodeHash); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatal("код должен быть одноразовым")
	}

	issue(t, st, u.ID, s.ID, "a-"+client.ID, "g-"+client.ID, true, client.ID)
	tok, err := st.AccessToken(ctx, "a-"+client.ID)
	if err != nil || tok.ShopID != s.ID || tok.Scopes[0] != oauth.ScopeRead {
		t.Fatalf("access: %v %+v", err, tok)
	}
	if g, ok := st.GrantIDByToken(ctx, "r-a-"+client.ID); !ok || g != "g-"+client.ID {
		t.Fatalf("выдача по refresh: %q %v", g, ok)
	}

	grants, err := db.UserGrants(ctx, u.ID)
	if err != nil || len(grants) != 1 || grants[0].ClientName != "Claude" || grants[0].ShopName != s.Name {
		t.Fatalf("подключения: %v %+v", err, grants)
	}

	rt, err := st.TakeRefreshToken(ctx, "r-a-"+client.ID)
	if err != nil || rt.ShopID != s.ID {
		t.Fatalf("refresh: %v %+v", err, rt)
	}
	if _, err := st.TakeRefreshToken(ctx, "r-a-"+client.ID); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatal("refresh должен ротироваться")
	}
	n, err := st.RevokeGrant(ctx, "g-"+client.ID)
	if err != nil || n != 1 {
		t.Fatalf("отзыв: %d %v", n, err)
	}
	if _, err := st.AccessToken(ctx, "a-"+client.ID); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatal("отозванный токен жив")
	}
}

func TestDeletingShopKillsItsTokens(t *testing.T) {
	db := pgtest.Open(t)
	st := db.OAuth()
	u := newUser(t, db)
	s := newShop(t, db, u.ID, "333")
	h := pgtest.Email("tok")
	issue(t, st, u.ID, s.ID, h, "g-"+h, true)
	if _, err := db.CreateAPIToken(ctx, u.ID, s.ID, "скрипт", "api-"+h, []string{"read"}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteShop(ctx, u.ID, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AccessToken(ctx, h); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatal("токен удалённого магазина продолжает работать")
	}
	if _, ok := st.GrantIDByToken(ctx, "r-"+h); ok {
		t.Fatal("refresh удалённого магазина остался")
	}
	if _, err := db.APITokenByHash(ctx, "api-"+h); !errors.Is(err, pgstore.ErrNotFound) {
		t.Fatal("API-токен удалённого магазина остался")
	}
}

func TestDisabledUserLosesAccessImmediately(t *testing.T) {
	db := pgtest.Open(t)
	st := db.OAuth()
	u := newUser(t, db)
	s := newShop(t, db, u.ID, "444")
	h := pgtest.Email("dis")
	issue(t, st, u.ID, s.ID, h, "g-"+h, true)
	if _, err := db.CreateAPIToken(ctx, u.ID, s.ID, "скрипт", "api-"+h, []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWebSession(ctx, "web-"+h, u.ID, "csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := db.SetDisabled(ctx, u.Email, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AccessToken(ctx, h); err == nil {
		t.Error("access заблокированного работает")
	}
	if _, err := st.TakeRefreshToken(ctx, "r-"+h); err == nil {
		t.Error("refresh заблокированного работает")
	}
	if _, err := db.APITokenByHash(ctx, "api-"+h); err == nil {
		t.Error("API-токен заблокированного работает")
	}
	if _, err := db.WebSession(ctx, "web-"+h); err == nil {
		t.Error("сессия кабинета заблокированного работает")
	}
	if err := db.SetDisabled(ctx, "нет-такого@example.com", true); !errors.Is(err, pgstore.ErrNotFound) {
		t.Errorf("блокировка несуществующего: %v", err)
	}
}

func TestAPITokenCannotTargetForeignShop(t *testing.T) {
	db := pgtest.Open(t)
	alice, bob := newUser(t, db), newUser(t, db)
	bobShop := newShop(t, db, bob.ID, "555")
	if _, err := db.CreateAPIToken(ctx, alice.ID, bobShop.ID, "x", pgtest.Email("h"), []string{"read"}); !errors.Is(err, pgstore.ErrNotFound) {
		t.Fatalf("токен к чужому магазину создан: %v", err)
	}

	aliceShop := newShop(t, db, alice.ID, "556")
	h := pgtest.Email("api")
	id, err := db.CreateAPIToken(ctx, alice.ID, aliceShop.ID, "PrintPipe", h, []string{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.APITokenByHash(ctx, h)
	if err != nil || tok.ShopID != aliceShop.ID || len(tok.Scopes) != 2 {
		t.Fatalf("токен: %v %+v", err, tok)
	}
	list, _ := db.APITokens(ctx, alice.ID)
	if len(list) != 1 || list[0].LastUsedAt == nil || list[0].ShopName != aliceShop.Name {
		t.Fatalf("список токенов: %+v", list)
	}
	if err := db.DeleteAPIToken(ctx, bob.ID, id); !errors.Is(err, pgstore.ErrNotFound) {
		t.Fatal("чужой токен отозван")
	}
}

func TestUsageAggregatesPerDay(t *testing.T) {
	db := pgtest.Open(t)
	u := newUser(t, db)
	for i := 0; i < 3; i++ {
		if err := db.RecordUsage(ctx, u.ID, 1, "ozon_product_list", i == 2); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.RecordUsage(ctx, u.ID, 1, "ozon_status", false)
	n, err := db.UsageSince(ctx, u.ID, 30)
	if err != nil || n != 4 {
		t.Fatalf("вызовов %d, %v", n, err)
	}
}

func TestPasswordChangeClosesOtherSessions(t *testing.T) {
	db := pgtest.Open(t)
	u := newUser(t, db)
	exp := time.Now().Add(time.Hour)
	_ = db.CreateWebSession(ctx, "keep-"+u.Email, u.ID, "c", exp)
	_ = db.CreateWebSession(ctx, "drop-"+u.Email, u.ID, "c", exp)

	if err := db.SetPassword(ctx, u.ID, "new-hash", "keep-"+u.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WebSession(ctx, "keep-"+u.Email); err != nil {
		t.Error("текущая сессия должна остаться")
	}
	if _, err := db.WebSession(ctx, "drop-"+u.Email); err == nil {
		t.Error("остальные сессии должны закрыться")
	}
}

func TestDeleteUserRemovesEverything(t *testing.T) {
	db := pgtest.Open(t)
	u := newUser(t, db)
	s := newShop(t, db, u.ID, "666")
	h := pgtest.Email("del")
	issue(t, db.OAuth(), u.ID, s.ID, h, "g-"+h, false)

	if err := db.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Shop(ctx, u.ID, s.ID); !errors.Is(err, pgstore.ErrNotFound) {
		t.Error("магазин пережил удаление пользователя")
	}
	if _, err := db.OAuth().AccessToken(ctx, h); err == nil {
		t.Error("токен пережил удаление пользователя")
	}
}

func TestCleanupRemovesExpired(t *testing.T) {
	db := pgtest.Open(t)
	u := newUser(t, db)
	h := pgtest.Email("old")
	_ = db.CreateWebSession(ctx, h, u.ID, "c", time.Now().Add(-time.Minute))
	if err := db.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WebSession(ctx, h); err == nil {
		t.Error("истёкшая сессия жива")
	}
}

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
)

// hashPassword считает хеш пароля владельца.
//
// Пароль читается со stdin, а не из аргумента командной строки:
// аргументы видны любому пользователю через ps и оседают в истории
// оболочки.
func hashPassword() {
	fmt.Fprint(os.Stderr, "Пароль владельца (минимум 12 символов): ")

	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		fatal("пароль не прочитан")
	}
	password := strings.TrimSpace(sc.Text())

	hash, err := oauth.HashPassword(password)
	if err != nil {
		fatal(err.Error())
	}

	fmt.Println(hash)
	fmt.Fprintln(os.Stderr,
		"\nПоложите строку в OZON_OWNER_PASSWORD_HASH.\n"+
			"Сам пароль нигде не хранится — вы будете вводить его на экране согласия\n"+
			"при подключении каждого нового устройства.")
}

// listGrants показывает действующие выдачи.
//
// Это ответ на вопрос «какие устройства сейчас имеют доступ» — тот
// самый, на который прежняя схема со статическим секретом ответить
// не могла в принципе.
func listGrants() {
	store, err := oauth.NewStore(env("OZON_OAUTH_STORE", "/var/lib/ozon-seller-mcp/oauth.json"))
	if err != nil {
		fatal("хранилище OAuth: " + err.Error())
	}

	grants := store.Grants()
	if len(grants) == 0 {
		fmt.Println("Действующих подключений нет.")
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ВЫДАЧА\tПРИЛОЖЕНИЕ\tПРАВА\tДЕЙСТВУЕТ ДО")
	for _, g := range grants {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			shortID(g.GrantID),
			g.ClientName,
			strings.Join(g.Scopes, " "),
			g.ExpiresAt.Format(time.DateTime))
	}
	_ = tw.Flush()

	fmt.Println("\nОтозвать: ozon-seller-mcp --revoke <ВЫДАЧА>")
}

// revokeGrant отзывает выдачу по её началу.
func revokeGrant(prefix string) {
	store, err := oauth.NewStore(env("OZON_OAUTH_STORE", "/var/lib/ozon-seller-mcp/oauth.json"))
	if err != nil {
		fatal("хранилище OAuth: " + err.Error())
	}

	var matches []oauth.Grant
	for _, g := range store.Grants() {
		if strings.HasPrefix(g.GrantID, prefix) {
			matches = append(matches, g)
		}
	}

	switch len(matches) {
	case 0:
		fatal("выдача не найдена: " + prefix)
	case 1:
		// продолжаем
	default:
		// Отзывать наугад нельзя: можно отключить не то устройство.
		fatal(fmt.Sprintf("под %q подходит %d выдач — уточните", prefix, len(matches)))
	}

	n, err := store.RevokeGrant(matches[0].GrantID)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("Отозвано: %s (%s), удалено токенов: %d\n",
		matches[0].ClientName, shortID(matches[0].GrantID), n)
	fmt.Println("Устройство потеряет доступ немедленно.")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

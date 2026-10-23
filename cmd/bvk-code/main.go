package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"bot-bvk/internal/domain"
)

func main() {
	bot := flag.String("bot", "", "Telegram bot username for a shareable link")
	flag.Parse()
	code, err := domain.GenerateCompanyCode()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Код компании:", code)
	if name := strings.TrimPrefix(strings.TrimSpace(*bot), "@"); name != "" {
		plain, _ := domain.NormalizeCompanyCode(code)
		fmt.Printf("Ссылка: https://t.me/%s?start=join_%s\n", name, plain)
	}
}

# Example App — Go

Même API que `example-app-nodejs`, implémentée en Go avec la stdlib `net/http`.

## Lancer en local

```bash
cp .env.example .env
go mod tidy
go run .
```

## Déployer sur Clever Cloud

```bash
clever create --type go
clever addon create postgresql-addon --plan dev
clever service link-addon <addon-id>
clever env set APP_NAME=<votre-nom>
git push clever main
```
# Estágio de Build
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copia os arquivos de módulo e baixa as dependências
COPY go.mod ./
RUN go mod download

# Copia o código fonte
COPY . .

# Compila o binário estático
RUN CGO_ENABLED=0 GOOS=linux go build -o /api-sppo-proxy main.go

# Estágio Final (mínimo)
FROM alpine:latest

WORKDIR /

# Instala certificados CA (necessários para chamadas HTTPS para a SPPO)
RUN apk --no-cache add ca-certificates

COPY --from=builder /api-sppo-proxy /api-sppo-proxy

# Porta padrão do Render
EXPOSE 8080

CMD ["/api-sppo-proxy"]

# Estágio de Build
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Instala certificados de CA para permitir que o go mod baixe dependências
RUN apk --no-cache add ca-certificates git

# Copia os arquivos de módulo
COPY go.mod ./
RUN go mod download

# Copia o código fonte
COPY . .

# Limpa e garante que as dependências estejam corretas
RUN go mod tidy

# Compila o binário estático no diretório atual
RUN CGO_ENABLED=0 GOOS=linux go build -o api-sppo-proxy .

# Estágio Final
FROM alpine:latest

WORKDIR /

# Reinstala certificados no estágio final para chamadas HTTPS
RUN apk --no-cache add ca-certificates

# Copia o binário gerado na etapa anterior
COPY --from=builder /app/api-sppo-proxy /api-sppo-proxy

# Porta padrão do Render
EXPOSE 8080

CMD ["/api-sppo-proxy"]

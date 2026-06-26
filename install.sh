#!/usr/bin/env bash
# ============================================================
#  LocalAI — авто-установщик для Linux
#  Поддерживает: Ubuntu, Debian, CentOS, RHEL, Fedora, Arch
#
#  Установка одной командой:
#  curl -sSL https://raw.githubusercontent.com/Fipoh455r/Progi/main/install.sh | bash
# ============================================================

set -euo pipefail

# --- Цвета для вывода ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

REPO_RAW="https://raw.githubusercontent.com/Fipoh455r/Progi/main"
INSTALL_DIR="${LOCALAI_DIR:-$HOME/.localai}"
WEBUI_PORT="${WEBUI_PORT:-3000}"

# ============================================================
print_banner() {
    echo -e "${CYAN}"
    echo "  ██╗      ██████╗  ██████╗ █████╗ ██╗      █████╗ ██╗"
    echo "  ██║     ██╔═══██╗██╔════╝██╔══██╗██║     ██╔══██╗██║"
    echo "  ██║     ██║   ██║██║     ███████║██║     ███████║██║"
    echo "  ██║     ██║   ██║██║     ██╔══██║██║     ██╔══██║██║"
    echo "  ███████╗╚██████╔╝╚██████╗██║  ██║███████╗██║  ██║██║"
    echo "  ╚══════╝ ╚═════╝  ╚═════╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═╝"
    echo -e "${NC}"
    echo -e "${BLUE}  Локальный AI-ассистент без облака${NC}"
    echo "  ================================================"
    echo ""
}

log_info()  { echo -e "${GREEN}[✓]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[!]${NC} $1"; }
log_error() { echo -e "${RED}[✗]${NC} $1"; }
log_step()  { echo -e "${BLUE}[→]${NC} $1"; }

# ============================================================
check_root() {
    if [[ $EUID -eq 0 ]]; then
        log_warn "Запущен от root. Рекомендуется запускать от обычного пользователя."
    fi
}

detect_os() {
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        OS_ID="${ID:-unknown}"
        OS_FAMILY="${ID_LIKE:-$OS_ID}"
    elif command -v lsb_release &>/dev/null; then
        OS_ID=$(lsb_release -si | tr '[:upper:]' '[:lower:]')
        OS_FAMILY="$OS_ID"
    else
        OS_ID="unknown"
        OS_FAMILY="unknown"
    fi

    case "$OS_FAMILY" in
        *debian*|*ubuntu*) PKG_MANAGER="apt"  ;;
        *fedora*|*rhel*|*centos*) PKG_MANAGER="dnf" ;;
        *arch*)            PKG_MANAGER="pacman" ;;
        *)
            case "$OS_ID" in
                ubuntu|debian|raspbian|linuxmint|pop) PKG_MANAGER="apt" ;;
                centos|rhel|fedora|rocky|almalinux)   PKG_MANAGER="dnf" ;;
                arch|manjaro|endeavouros)              PKG_MANAGER="pacman" ;;
                *)
                    log_warn "Дистрибутив '$OS_ID' не распознан, попробуем apt..."
                    PKG_MANAGER="apt"
                    ;;
            esac
            ;;
    esac

    log_info "Дистрибутив: ${OS_ID} (менеджер пакетов: ${PKG_MANAGER})"
}

# ============================================================
install_docker() {
    log_step "Устанавливаю Docker..."

    case "$PKG_MANAGER" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            sudo apt-get update -qq
            sudo apt-get install -y -qq ca-certificates curl gnupg lsb-release

            # Официальный репозиторий Docker
            sudo install -m 0755 -d /etc/apt/keyrings
            curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
                | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg 2>/dev/null || \
            curl -fsSL https://download.docker.com/linux/debian/gpg \
                | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg 2>/dev/null
            sudo chmod a+r /etc/apt/keyrings/docker.gpg

            # Добавляем репозиторий
            ARCH=$(dpkg --print-architecture)
            CODENAME=$(. /etc/os-release && echo "$VERSION_CODENAME")
            echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu ${CODENAME} stable" \
                | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

            sudo apt-get update -qq
            sudo apt-get install -y -qq docker-ce docker-ce-cli containerd.io \
                docker-buildx-plugin docker-compose-plugin
            ;;
        dnf)
            sudo dnf -y install dnf-plugins-core
            sudo dnf config-manager --add-repo \
                https://download.docker.com/linux/fedora/docker-ce.repo
            sudo dnf install -y docker-ce docker-ce-cli containerd.io \
                docker-buildx-plugin docker-compose-plugin
            ;;
        pacman)
            sudo pacman -Sy --noconfirm docker docker-compose
            ;;
    esac

    # Запускаем и добавляем в автозапуск
    sudo systemctl enable --now docker

    # Добавляем текущего пользователя в группу docker (без sudo)
    if [[ $EUID -ne 0 ]]; then
        sudo usermod -aG docker "$USER"
        log_warn "Пользователь добавлен в группу 'docker'. Для применения нужен повторный вход."
        log_warn "Продолжаю установку через sudo docker..."
        DOCKER_CMD="sudo docker"
    fi

    log_info "Docker установлен"
}

check_docker() {
    if command -v docker &>/dev/null; then
        DOCKER_VERSION=$(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',')
        log_info "Docker найден: v${DOCKER_VERSION}"
        DOCKER_CMD="docker"

        # Проверяем доступ без sudo
        if ! docker info &>/dev/null 2>&1; then
            log_warn "Docker требует sudo..."
            DOCKER_CMD="sudo docker"
        fi
    else
        log_warn "Docker не найден, устанавливаю..."
        install_docker
        DOCKER_CMD="sudo docker"
    fi
}

check_compose() {
    # Проверяем docker compose (plugin) или docker-compose (standalone)
    if $DOCKER_CMD compose version &>/dev/null 2>&1; then
        COMPOSE_CMD="$DOCKER_CMD compose"
        log_info "Docker Compose plugin найден"
    elif command -v docker-compose &>/dev/null; then
        COMPOSE_CMD="docker-compose"
        log_info "docker-compose найден"
    else
        log_warn "Docker Compose не найден, устанавливаю..."
        case "$PKG_MANAGER" in
            apt) sudo apt-get install -y -qq docker-compose-plugin ;;
            dnf) sudo dnf install -y docker-compose-plugin ;;
            pacman) sudo pacman -S --noconfirm docker-compose ;;
        esac
        COMPOSE_CMD="$DOCKER_CMD compose"
    fi
}

detect_gpu() {
    GPU_FLAG=""
    if command -v nvidia-smi &>/dev/null && nvidia-smi &>/dev/null 2>&1; then
        log_info "Обнаружен NVIDIA GPU — включаю ускорение"
        GPU_FLAG="gpu"
    else
        log_info "GPU не обнаружен — использую CPU"
        GPU_FLAG="cpu"
    fi
}

check_resources() {
    # Проверка минимальных ресурсов
    RAM_MB=$(awk '/MemTotal/ {printf "%.0f", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
    DISK_FREE_GB=$(df -BG "$INSTALL_DIR" 2>/dev/null | awk 'NR==2{print $4}' | tr -d 'G' || echo 0)

    log_info "RAM: ${RAM_MB}MB | Свободное место: ${DISK_FREE_GB}GB"

    if [[ "$RAM_MB" -lt 512 ]]; then
        log_warn "Мало RAM (${RAM_MB}MB). Минимум: 512MB. Возможны тормоза."
    fi

    if [[ "$DISK_FREE_GB" -lt 2 ]]; then
        log_error "Недостаточно места на диске (${DISK_FREE_GB}GB). Нужно минимум 2GB."
        exit 1
    fi
}

# ============================================================
setup_files() {
    log_step "Создаю директорию установки: ${INSTALL_DIR}"
    mkdir -p "$INSTALL_DIR"

    # Скачиваем docker-compose файлы
    log_step "Скачиваю конфигурацию..."

    if [[ "$GPU_FLAG" == "gpu" ]]; then
        COMPOSE_FILE="docker-compose.yml"
    else
        COMPOSE_FILE="docker-compose.cpu.yml"
    fi

    curl -fsSL "${REPO_RAW}/${COMPOSE_FILE}" -o "${INSTALL_DIR}/docker-compose.yml"
    curl -fsSL "${REPO_RAW}/.env.example"    -o "${INSTALL_DIR}/.env.example"

    # Создаём .env если не существует
    if [[ ! -f "${INSTALL_DIR}/.env" ]]; then
        cp "${INSTALL_DIR}/.env.example" "${INSTALL_DIR}/.env"
        # Генерируем случайный секретный ключ
        SECRET=$(cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 32 | head -n 1 2>/dev/null || echo "change-me-$(date +%s)")
        sed -i "s/localai-secret-change-me-please/${SECRET}/" "${INSTALL_DIR}/.env"
        log_info "Создан .env с случайным секретным ключом"
    else
        log_info ".env уже существует, не перезаписываю"
    fi

    log_info "Файлы сохранены в ${INSTALL_DIR}"
}

# ============================================================
create_systemd_service() {
    if ! command -v systemctl &>/dev/null; then
        log_warn "systemd не найден, пропускаю создание сервиса"
        return
    fi

    log_step "Создаю systemd сервис для автозапуска..."

    # Определяем путь к docker/docker-compose
    DOCKER_BIN=$(command -v docker || echo "/usr/bin/docker")

    sudo tee /etc/systemd/system/localai.service > /dev/null << EOF
[Unit]
Description=LocalAI — локальный AI-ассистент
Documentation=https://github.com/Fipoh455r/Progi
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=${INSTALL_DIR}
ExecStart=${DOCKER_BIN} compose up -d --remove-orphans
ExecStop=${DOCKER_BIN} compose down
TimeoutStartSec=300
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

    sudo systemctl daemon-reload
    sudo systemctl enable localai.service
    log_info "Сервис 'localai' создан и включён в автозапуск"
}

create_cli_shortcut() {
    log_step "Создаю команду 'localai' в системе..."

    sudo tee /usr/local/bin/localai > /dev/null << SCRIPT
#!/usr/bin/env bash
# Управление LocalAI
INSTALL_DIR="${INSTALL_DIR}"
cd "\$INSTALL_DIR"

case "\${1:-help}" in
    start)   docker compose up -d && echo "LocalAI запущен: http://localhost:${WEBUI_PORT}" ;;
    stop)    docker compose down ;;
    restart) docker compose restart ;;
    logs)    docker compose logs -f "\${2:-webui}" ;;
    update)
        echo "Обновление LocalAI..."
        curl -fsSL "${REPO_RAW}/docker-compose.yml" -o docker-compose.yml
        docker compose pull
        docker compose up -d --remove-orphans
        echo "Обновление завершено"
        ;;
    status)  docker compose ps ;;
    models)
        echo "Доступные модели:"
        curl -s http://localhost:11434/api/tags | python3 -c "
import sys, json
data = json.load(sys.stdin)
for m in data.get('models', []):
    size = m.get('size', 0) // 1024 // 1024
    print(f'  - {m[\"name\"]} ({size}MB)')
" 2>/dev/null || echo "  Ollama недоступен"
        ;;
    pull)
        MODEL="\${2:-qwen2.5:0.5b}"
        echo "Загрузка модели: \$MODEL"
        curl -X POST http://localhost:11434/api/pull \
            -H 'Content-Type: application/json' \
            -d "{\"name\": \"\$MODEL\"}" \
            --no-buffer
        ;;
    help|*)
        echo ""
        echo "LocalAI — локальный AI-ассистент"
        echo ""
        echo "Использование: localai <команда>"
        echo ""
        echo "Команды:"
        echo "  start    — запустить"
        echo "  stop     — остановить"
        echo "  restart  — перезапустить"
        echo "  status   — статус контейнеров"
        echo "  logs     — показать логи (localai logs webui)"
        echo "  models   — список загруженных моделей"
        echo "  pull <модель> — скачать модель (пр: localai pull llama3.2:1b)"
        echo "  update   — обновить до последней версии"
        echo ""
        echo "Веб-интерфейс: http://localhost:${WEBUI_PORT}"
        echo ""
        ;;
esac
SCRIPT

    sudo chmod +x /usr/local/bin/localai
    log_info "Команда 'localai' доступна в терминале"
}

# ============================================================
start_services() {
    log_step "Запускаю контейнеры..."
    cd "$INSTALL_DIR"
    $COMPOSE_CMD up -d --remove-orphans

    log_step "Жду готовности Ollama..."
    MAX_WAIT=60
    WAITED=0
    while [[ $WAITED -lt $MAX_WAIT ]]; do
        if curl -sf http://localhost:11434/api/tags &>/dev/null; then
            log_info "Ollama готов"
            break
        fi
        sleep 2
        WAITED=$((WAITED + 2))
    done

    if [[ $WAITED -ge $MAX_WAIT ]]; then
        log_warn "Ollama ещё запускается, дождись и проверь: localai status"
    fi
}

print_success() {
    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  LocalAI успешно установлен!${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo ""
    echo -e "  ${CYAN}Веб-интерфейс:${NC}  http://localhost:${WEBUI_PORT}"
    echo -e "  ${CYAN}API Ollama:${NC}     http://localhost:11434"
    echo -e "  ${CYAN}Файлы:${NC}          ${INSTALL_DIR}"
    echo ""
    echo -e "  ${YELLOW}Управление:${NC}"
    echo "    localai start   — запуск"
    echo "    localai stop    — остановка"
    echo "    localai status  — статус"
    echo "    localai update  — обновление"
    echo "    localai pull mistral:7b  — скачать другую модель"
    echo ""
    echo -e "  ${YELLOW}Первый вход:${NC} зарегистрируйся в веб-интерфейсе"
    echo "  (первый аккаунт автоматически становится админом)"
    echo ""
}

# ============================================================
#  ГЛАВНАЯ ЛОГИКА
# ============================================================
main() {
    print_banner
    check_root
    detect_os
    check_resources
    check_docker
    check_compose
    detect_gpu
    setup_files
    create_systemd_service
    create_cli_shortcut
    start_services
    print_success
}

main "$@"

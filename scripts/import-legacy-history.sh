#!/bin/sh
set -eu

usage() {
    cat <<'EOF'
Usage:
  import-legacy-history.sh start SOURCE_DB DEPLOY_DIR [--include-long-term]
  import-legacy-history.sh status DEPLOY_DIR
  import-legacy-history.sh logs DEPLOY_DIR

start returns immediately. The worker preflights the source, stops Komari,
backs up the active databases, imports raw history, and starts Komari again.
records_long_term is excluded unless --include-long-term is explicitly given.
EOF
}

absolute_path() {
    target=$1
    if command -v realpath >/dev/null 2>&1; then
        realpath "$target"
    else
        directory=$(dirname "$target")
        base=$(basename "$target")
        (cd "$directory" && printf '%s/%s\n' "$(pwd -P)" "$base")
    fi
}

timestamp() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

log() {
    printf '[%s] %s\n' "$(timestamp)" "$*"
}

start_worker() {
    [ "$#" -ge 2 ] || { usage; exit 2; }
    source_db=$(absolute_path "$1")
    deploy_dir=$(absolute_path "$2")
    include_long_term=${3:-}
    [ -f "$source_db" ] || { echo "Source database not found: $source_db" >&2; exit 1; }
    [ -f "$deploy_dir/docker-compose.yml" ] || { echo "docker-compose.yml not found in $deploy_dir" >&2; exit 1; }
    case "$include_long_term" in
        ""|--include-long-term) ;;
        *) echo "Unknown option: $include_long_term" >&2; exit 2 ;;
    esac

    script_path=$(absolute_path "$0")
    pid_file="$deploy_dir/import-legacy-history.pid"
    log_file="$deploy_dir/import-legacy-history.log"
    if [ -f "$pid_file" ]; then
        old_pid=$(cat "$pid_file" 2>/dev/null || true)
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            echo "Import already running with PID $old_pid" >&2
            exit 1
        fi
        rm -f "$pid_file"
    fi
    if [ -f "$log_file" ]; then
        mv "$log_file" "$log_file.$(date -u '+%Y%m%d-%H%M%S')"
    fi
    nohup "$script_path" __worker "$source_db" "$deploy_dir" "$include_long_term" >>"$log_file" 2>&1 </dev/null &
    worker_pid=$!
    printf '%s\n' "$worker_pid" >"$pid_file"
    echo "Legacy history import started with PID $worker_pid"
    echo "Log: $log_file"
    echo "Follow: $script_path logs $deploy_dir"
}

show_status() {
    [ "$#" -eq 1 ] || { usage; exit 2; }
    deploy_dir=$(absolute_path "$1")
    pid_file="$deploy_dir/import-legacy-history.pid"
    log_file="$deploy_dir/import-legacy-history.log"
    if [ -f "$pid_file" ]; then
        worker_pid=$(cat "$pid_file" 2>/dev/null || true)
        if [ -n "$worker_pid" ] && kill -0 "$worker_pid" 2>/dev/null; then
            echo "running (PID $worker_pid)"
            exit 0
        fi
    fi
    if [ -f "$log_file" ] && grep -q 'Import worker completed successfully' "$log_file"; then
        echo "completed"
    elif [ -f "$log_file" ]; then
        echo "not running; inspect $log_file"
    else
        echo "not started"
    fi
}

show_logs() {
    [ "$#" -eq 1 ] || { usage; exit 2; }
    deploy_dir=$(absolute_path "$1")
    log_file="$deploy_dir/import-legacy-history.log"
    [ -f "$log_file" ] || { echo "Log not found: $log_file" >&2; exit 1; }
    tail -n 100 -f "$log_file"
}

restore_backup() {
    backup_dir=$1
    data_dir=$2
    failed_dir="$backup_dir/failed-target"
    mkdir -p "$failed_dir"
    for name in komari.db komari.db-wal komari.db-shm metrics.db metrics.db-wal metrics.db-shm; do
        if [ -e "$data_dir/$name" ]; then
            mv "$data_dir/$name" "$failed_dir/$name"
        fi
        if [ -e "$backup_dir/$name" ]; then
            cp -a "$backup_dir/$name" "$data_dir/$name"
        fi
    done
}

run_worker() {
    [ "$#" -eq 3 ] || exit 2
    source_db=$1
    deploy_dir=$2
    include_long_term=$3
    data_dir="$deploy_dir/data"
    pid_file="$deploy_dir/import-legacy-history.pid"
    trap 'rm -f "$pid_file"' EXIT INT TERM

    log "Import worker started"
    log "Source: $source_db"
    log "Deployment: $deploy_dir"
    log "Long-term aggregate import: ${include_long_term:-disabled}"
    cd "$deploy_dir"
    command -v docker >/dev/null 2>&1 || { log "ERROR: docker is not installed"; exit 1; }
    docker compose config --quiet

    source_size_before=$(wc -c <"$source_db")
    sleep 5
    source_size_after=$(wc -c <"$source_db")
    [ "$source_size_before" = "$source_size_after" ] || { log "ERROR: source database is still changing"; exit 1; }
    if command -v sqlite3 >/dev/null 2>&1; then
        table_count=$(sqlite3 "file:$source_db?mode=ro&immutable=1" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('records','gpu_records','ping_records');")
        [ "$table_count" -gt 0 ] || { log "ERROR: source has no legacy monitoring tables"; exit 1; }
        if [ "${KOMARI_IMPORT_FULL_CHECK:-0}" = "1" ]; then
            log "Running full SQLite integrity check before stopping Komari"
            check_result=$(sqlite3 "file:$source_db?mode=ro&immutable=1" "PRAGMA quick_check;")
            [ "$check_result" = "ok" ] || { log "ERROR: source integrity check failed: $check_result"; exit 1; }
        fi
    fi

    log "Pulling the current Komari image while the service remains online"
    docker compose pull komari
    image=$(docker compose config --images | sed -n '1p')
    [ -n "$image" ] || { log "ERROR: failed to resolve Komari image"; exit 1; }

    log "Stopping Komari for offline import"
    docker compose stop komari
    backup_dir="$deploy_dir/backup/history-import-$(date -u '+%Y%m%d-%H%M%S')"
    mkdir -p "$backup_dir"
    for name in komari.db komari.db-wal komari.db-shm metrics.db metrics.db-wal metrics.db-shm; do
        if [ -e "$data_dir/$name" ]; then
            cp -a "$data_dir/$name" "$backup_dir/$name"
        fi
    done
    log "Offline database backup: $backup_dir"

    import_flag=
    if [ "$include_long_term" = "--include-long-term" ]; then
        import_flag=--include-long-term
    fi
    set +e
    docker run --rm \
        --name komari-history-import \
        --cpus "${KOMARI_IMPORT_CPUS:-0.30}" \
        --cpu-shares 128 \
        --blkio-weight 100 \
        -v "$data_dir:/app/data" \
        -v "$source_db:/import/komari.db:ro" \
        -w /app \
        "$image" \
        import-legacy-history \
        --source /import/komari.db \
        --database /app/data/komari.db \
        --confirm-offline \
        $import_flag
    import_status=$?
    set -e

    if [ "$import_status" -ne 0 ]; then
        log "ERROR: import exited with status $import_status; restoring offline backup"
        restore_backup "$backup_dir" "$data_dir"
        docker compose up -d komari
        log "Original databases restored and Komari restarted"
        exit "$import_status"
    fi

    log "Import finished; starting Komari"
    docker compose up -d komari
    docker compose ps komari
    log "Import worker completed successfully"
}

command=${1:-}
case "$command" in
    start)
        shift
        start_worker "$@"
        ;;
    status)
        shift
        show_status "$@"
        ;;
    logs)
        shift
        show_logs "$@"
        ;;
    __worker)
        shift
        run_worker "$@"
        ;;
    *)
        usage
        exit 2
        ;;
esac

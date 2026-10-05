# Contexto: Transcrição de áudio

Transforma voice messages em `content` pesquisável. **Opt-in** — desligado até configurar engine. Sem engine, sweep é no-op limpo (não marca áudios; ativar depois pega todos).

## Engines (`transcribe.py`)

- `TRANSCRIPTION_ENGINE=local|api`.
- **local**: `WHISPER_CLI` + `WHISPER_MODEL` (whisper.cpp). Privado, sem custo. Sem defaults: ambos vazios → `engine_ready()` = not configured (sweep no-op); setar os dois no env.
- **api**: OpenAI-compatible. `TRANSCRIPTION_API_KEY`, `TRANSCRIPTION_API_BASE` (default OpenAI; Groq = `https://api.groq.com/openai/v1`), `TRANSCRIPTION_API_MODEL` (`whisper-1` OpenAI; `whisper-large-v3` Groq — `whisper-1` na Groq dá 404). Áudio sai da máquina.
- `TRANSCRIPTION_PROMPT` enviesa ambos pra termos de domínio.
- `engine_ready()` retorna `(ok, reason)`; main() não faz NADA (content='') se não configurado.

## Armadilhas

- **medium transcreve foneticamente** ("Proteus" vs "Protheus"). **large-v3** é melhor pra termos TOTVS/Protheus/ADVPL. Este setup usa large-v3 + prompt de domínio.
- `write_content` usa `UPDATE` (não INSERT OR REPLACE) — seguro. O que apaga transcrição é o **bridge no history sync** (ver bridge-go).
- Três estados de content: texto real / sentinela / '' (vazio = retry no próximo sweep).
- `CDN_EXPIRY = 21 dias`. `_is_expired(ts)` discrimina falha transitória vs permanente. None/unparseable = assumido expirado (não deixa row em retry eterno).

## Fluxos

- **Backfill**: `python3 transcribe.py` (idempotente, resumível) — só áudios ainda baixáveis.
- **Recovery** (`recover_audios.py`): áudios `[áudio indisponível…]` expirados do CDN. Pede phone re-upload via media retry. **Phone online obrigatório.** Faz scrape do `bridge.log` → bridge precisa logar em arquivo + `WHATSAPP_BRIDGE_LOG` apontando pra ele. Throttled em batches (flood de retry = rate-limit).
- **Sweep contínuo**: ticker 5min na bridge shells out `transcribe.py` (processo separado, lockfile evita overlap). Novos áudios pesquisáveis em ~5min.

## Importante para scripts

- Scripts são processos separados → leem env do **próprio shell**. `source transcription.env` antes de rodar (o env da bridge não chega neles).
- Testes: `test_transcribe.py` (`_is_expired`, `_strip_accents`).

# gs3

AWS CLI **1.31.13**의 `aws s3` 명령을 그대로 대체하는 Go 바이너리입니다.
플래그, 출력, 종료 코드는 AWS CLI와 같고, 전송과 목록 조회는 goroutine으로 병렬 처리해 훨씬 빠릅니다.

- 대상: NCP Object Storage, Ncloud Storage 등 S3 호환 스토리지 (AWS S3도 동작)
- 구현: `aws-sdk-go-v2`, 단일 정적 바이너리, 외부 런타임 불필요
- 설계와 진행 계획: [docs/PLAN.md](docs/PLAN.md), 호환 기준이 된 동작 스펙: [docs/compat-spec.md](docs/compat-spec.md)

## 사용법

```sh
make build                      # bin/gs3
bin/gs3 s3 sync ./data s3://bucket/prefix --delete --endpoint-url https://kr.object.ncloudstorage.com
```

`aws`와 같은 형태로 호출합니다. 기존 스크립트는 별칭이나 심볼릭 링크로 바꿔 끼울 수 있습니다.

```sh
alias aws=gs3            # 또는: ln -s /usr/local/bin/gs3 /usr/local/bin/aws
aws s3 ls s3://bucket/prefix/
```

- `gs3 s3 <명령> …`과 축약형 `gs3 <명령> …`을 모두 받습니다.
- 전역 옵션은 위치와 무관합니다: `gs3 --endpoint-url=X s3 ls`, `gs3 s3 --endpoint-url=X ls`, `gs3 s3 ls --endpoint-url X`.
- 자격 증명과 설정은 AWS CLI와 같은 곳에서 읽습니다: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION`, `AWS_ENDPOINT_URL`, `~/.aws/config`, `~/.aws/credentials`.

## 호환 범위

| 명령 | 지원 |
|---|---|
| `ls`, `cp`, `mv`, `rm`, `sync`, `mb`, `rb`, `presign`, `website` | 1.31.13의 모든 플래그 |
| 전역 옵션 | `--debug --endpoint-url --no-verify-ssl --no-paginate --output --query --profile --region --version --color --no-sign-request --ca-bundle --cli-read-timeout --cli-connect-timeout` |
| `s3api` 등 다른 서비스 | 지원하지 않음 |

호환성은 실제 AWS CLI 1.31.13과 나란히 실행해 확인합니다(아래 "차등 테스트"). 비교 항목은 종료 코드, 표준 출력, 표준 오류, 실행 후 버킷 상태(키·크기·내용·헤더), 로컬 파일 상태(경로·크기·내용·mtime)입니다.

### AWS CLI와 의도적으로 다른 점

| 항목 | AWS CLI 1.31.13 | gs3 |
|---|---|---|
| 동시 요청 수, 멀티파트 크기 | 10개, 8MiB | 컨테이너 CPU·메모리에 맞춘 기본값 (아래) |
| 재시도 | 최대 5회 | 최대 8회 (`AWS_MAX_ATTEMPTS`로 변경) |
| `rm --recursive`, `sync --delete` | 키마다 `DeleteObject` | 1,000개 묶음 `DeleteObjects`, 미지원이면 키별 삭제 |
| 대용량 S3→S3 복사 | 멀티파트 복사에서 Content-Type·메타데이터 유실 | 원본에서 읽어 유지 |
| `Expect: 100-continue` | 업로드마다 전송 | 전송하지 않음 |
| `presign` | AWS의 알려진 리전에서는 SigV2 | 항상 SigV4 (NCP 리전은 AWS CLI도 SigV4) |
| 진행률 줄 | 청크마다 갱신 | 같은 형식, 초당 5회 갱신 |
| 인자 오류 시 usage 문구 | "AWS CLI version 2…" 안내 포함 | 안내 문구 없음. 오류 줄은 동일 |
| `help` | man 페이지 | 간단한 요약과 공식 문서 링크 |
| `--debug` 로그 | botocore 형식 | SDK 형식 |
| `--version` | `aws-cli/1.31.13 Python/…` | `gs3/<버전> (aws-cli/1.31.13 compatible) …` |
| `file://`, `fileb://` 인자 | 위치 인자에도 적용 | 옵션 값에만 적용 |

### 요청 형태

스토리지에 보내는 요청은 botocore 1.33(AWS CLI 1.31.13이 쓰는 라이브러리)과 같게 맞췄습니다. 최신 SDK의 기본 동작 중 S3 호환 스토리지가 거부할 수 있는 것은 쓰지 않습니다.

- 사용자 지정 엔드포인트에서는 path-style 주소를 씁니다. `addressing_style = virtual`로 바꿀 수 있습니다.
- 업로드와 `DeleteObjects`에 `Content-MD5`를 붙입니다. `x-amz-checksum-crc32`와 aws-chunked 트레일러는 보내지 않습니다.
- SDK가 붙이는 `x-id` 쿼리 파라미터와 기본 `Content-Type` 헤더를 제거합니다.

## 성능 설정

기본값은 실행 환경의 cgroup 제한을 보고 정합니다.

| 설정 | AWS CLI | gs3 기본값 |
|---|---|---|
| 동시 요청 수 | 10 | `clamp(32 × CPU, 16, 128)`. 메모리 256Mi 이하면 최대 32, 512Mi 이하면 최대 64 |
| `multipart_threshold` | 8MiB | 32MiB |
| `multipart_chunksize` | 8MiB | 16MiB (S3→S3 복사 파트는 64MiB) |
| `max_queue_size` | 1000 | 2000 |

우선순위는 `GS3_*` 환경변수 > `~/.aws/config`의 `s3 =` 블록 > 기본값입니다.

```ini
[default]
s3 =
    max_concurrent_requests = 64
    multipart_chunksize = 32MB
    max_bandwidth = 100MB/s
    addressing_style = path
```

| 환경변수 | 의미 |
|---|---|
| `GS3_MAX_CONCURRENT_REQUESTS`, `GS3_MAX_QUEUE_SIZE` | 동시 요청 수, 작업 큐 크기 |
| `GS3_MULTIPART_THRESHOLD`, `GS3_MULTIPART_CHUNKSIZE` | 멀티파트 기준과 파트 크기 (`16MB` 형식) |
| `GS3_MAX_BANDWIDTH` | 전송 속도 상한 (`50MB/s` 형식) |
| `GS3_MAX_PARTS` | 멀티파트 최대 파트 수 (기본 10000) |
| `GS3_AWS_DEFAULTS=1` | AWS CLI와 같은 기본값 사용 |
| `GS3_CONTENT_MD5=0` | 업로드의 `Content-MD5` 생략 (CPU가 매우 적은 환경에서 더 빠름) |
| `GS3_BATCH_DELETE=0` | 묶음 삭제 끄기 |
| `GS3_ADAPTIVE=0` | 429/503 응답 시 동시성을 자동으로 낮추는 기능 끄기 |

메모리 사용량은 객체 수나 파일 크기와 무관합니다. 업로드는 파일 구간을 직접 읽고 다운로드는 받은 바이트를 파일의 해당 위치에 바로 쓰므로 파트를 메모리에 올리지 않습니다. 표준 입출력 스트리밍(`cp - s3://…`, `cp s3://… -`)만 8MiB 버퍼를 최대 4개 씁니다.

## 개발

```sh
make build          # bin/gs3
make test           # 단위·통합 테스트 (레이스 검출기)
make lint           # go vet + gofmt 확인
```

### 차등 테스트

같은 명령을 실제 AWS CLI 1.31.13과 gs3에 주고 결과를 비교합니다. 두 도구 모두 같은 컨테이너 안에서 실행되고, 저장소는 메모리 기반 가짜 S3 서버(`internal/s3fake`)를 씁니다.

```sh
make ref-image      # AWS CLI 1.31.13 기준 이미지 (python:3.11-slim + awscli==1.31.13)
make s3fake         # 다른 터미널에서 가짜 S3 서버 실행 (127.0.0.1:19090)
make compat         # test/compat/cases/*.json 전체 실행
```

- 시나리오 추가: `test/compat/cases/`의 JSON에 항목을 더합니다. 형식은 `test/compat/compare.py` 머리말에 있습니다.
- 실제 CLI가 무엇을 출력하는지 확인: `echo 's3 cp a' | test/compat/refrun.py`
- 파서 기준 데이터 갱신: `make ref-dump` (실제 CLI의 인자 테이블과 파싱 결과를 다시 뽑습니다)

### 스토리지 기능 점검

S3 호환 스토리지가 gs3 설계에 필요한 동작을 지원하는지 확인합니다. 지정한 버킷의 `gs3-probe-<난수>/` 아래에만 쓰고 끝나면 지웁니다.

```sh
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
export AWS_ENDPOINT_URL=https://kr.object.ncloudstorage.com AWS_DEFAULT_REGION=kr-standard
go run ./tools/probe -bucket <테스트 버킷>
```

### 구조

| 패키지 | 역할 |
|---|---|
| `internal/cli` | argparse 호환 파서 (3단계 파싱, 접두어 축약, `nargs`), 플래그 테이블 |
| `internal/command` | 명령 구현과 종료 코드 |
| `internal/awscfg` | 환경변수·설정 파일 해석 (botocore 순서, `s3 =` 블록) |
| `internal/s3client` | 클라이언트 생성: 엔드포인트, 서명, 재시도, HTTP 전송 계층 |
| `internal/listing` | 로컬·S3 목록 조회 (키의 바이트 순서) |
| `internal/filter` | `--include`/`--exclude` (Python `fnmatch` 호환) |
| `internal/syncer` | sync 판정 |
| `internal/transfer` | 전송 엔진: 동시성 제한, 멀티파트, 스트림, 묶음 삭제 |
| `internal/output` | 결과·진행률 출력, 오류 문구 |
| `internal/s3fake` | 테스트용 메모리 S3 서버 (요청 기록, 장애 주입) |

## 진행 상황

[docs/PLAN.md](docs/PLAN.md)의 마일스톤 기준입니다.

| 단계 | 상태 |
|---|---|
| M0 준비 | 완료. NCP 실제 엔드포인트 점검(`tools/probe`)은 테스트 버킷 정보가 필요해 미실행 |
| M1 파서와 단순 명령 | 완료 |
| M2 단일 요청 전송, M3 재귀 작업, M4 sync | 완료 |
| M5 대용량·스트림 | 완료 (멀티파트 업로드·복사, 범위 다운로드, 표준 입출력) |
| M6 견고성 | 일부: 재시도, 다운로드 이어받기, 적응형 동시성, 연결 유휴 타임아웃, 대역폭 제한 구현. 장애 주입 테스트는 일부만 |
| M7 성능 | 미착수: 병렬 목록 조회, 벤치마크로 기본값 확정, PGO |
| M8 배포 | 미착수: 실제 NCP 장시간 테스트, 워크플로 병행 실행, 릴리스 빌드·이미지 |

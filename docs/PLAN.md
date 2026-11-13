# gs3 — AWS CLI 1.31.13 `aws s3` 호환 고성능 Go 바이너리 구현 계획

## Context

- **배경**: Argo Workflow 스텝들이 `aws s3 sync / cp / rm / ls`를 약 270곳에서 호출한다. 대상은 NCP Object Storage, Ncloud Storage, 테스트용 MinIO이고 AWS S3는 쓰지 않는다. Python 기반 AWS CLI는 동시 요청 10개·8MiB 청크가 기본이라 느리고, 스크립트마다 3회 재시도·30초 대기·`--cli-read-timeout 300` 같은 보정이 손으로 붙어 있다.
- **목표**: `aws-sdk-go-v2` 기반 단일 정적 바이너리 `gs3`를 만들어 AWS CLI **1.31.13**의 `aws s3` 명령을 그대로 대체한다. 플래그·출력·종료 코드는 호환하고, goroutine 기반 병렬화로 전송과 목록 조회를 빠르게 한다.
- **확정된 결정**
  - 범위: `s3` 고수준 명령 9종(`ls, cp, mv, rm, sync, mb, rb, presign, website`)과 전역 옵션 전부. `s3api`는 제외.
  - 대상 스토리지: NCP Object Storage + Ncloud Storage. MinIO는 테스트용.
  - 워크로드: 작은 파일 다수와 대용량 파일 혼합.
  - 기본 성능값: AWS CLI보다 공격적으로 두되, `~/.aws/config`의 `s3 =` 블록에 명시된 값은 따른다.
  - 업로드 무결성: AWS CLI처럼 `Content-MD5`를 기본으로 보낸다. `GS3_CONTENT_MD5=0`으로 끌 수 있다.
  - NCP 테스트용 버킷과 자격 증명은 처음부터 쓸 수 있다. 첫 단계에서 두 서비스의 실제 동작을 점검한다.
- **현재 상태**: 저장소는 비어 있고 git 저장소도 아니다. 이 머신에는 Go와 AWS CLI가 없고 Homebrew와 Docker는 있다.
- **실행 환경**: gs3는 CPU 100m~4코어, 메모리 256Mi~8Gi 제한의 컨테이너에서 돈다. 메모리 사용량은 객체 수나 파트 크기와 무관해야 한다.

## 1. 호환성 기준

### 1.1 호출 형태

- `gs3 s3 <명령> …`: AWS CLI와 같은 형태. `aws`라는 이름의 심볼릭 링크나 `alias aws=gs3`로 바꿔 끼울 수 있다.
- `gs3 <명령> …`: 축약형도 받는다.
- 전역 옵션은 위치와 무관하게 인식한다. 워크플로에 세 형태가 모두 있다.
  - `gs3 --endpoint-url=X s3 sync …`
  - `gs3 s3 --endpoint-url=X sync …`
  - `gs3 s3 sync … --endpoint-url X`
- `s3api` 등 범위 밖 명령은 "지원하지 않는 명령" 오류를 내고 종료 코드 2로 끝낸다.

### 1.2 명령과 플래그 (1.31.13 소스에서 확인)

| 명령 | 위치 인자 | 플래그 |
|---|---|---|
| `ls` | `<S3Uri>` 또는 없음 | `--recursive --page-size --human-readable --summarize --request-payer` |
| `cp` | 경로 2개 | 공통 전송 플래그, `--metadata --metadata-directive --expected-size --recursive` |
| `mv` | 경로 2개 | 공통 전송 플래그, `--metadata --metadata-directive --recursive` |
| `sync` | 경로 2개 | 공통 전송 플래그, `--metadata --metadata-directive --size-only --exact-timestamps --delete` |
| `rm` | 경로 1개 | `--dryrun --quiet --recursive --request-payer --include --exclude --only-show-errors --page-size` |
| `mb` | 경로 1개 | 없음 |
| `rb` | 경로 1개 | `--force` |
| `presign` | 경로 1개 | `--expires-in` (기본 3600) |
| `website` | 경로 1개 | `--index-document --error-document` |

**공통 전송 플래그** (cp, mv, sync):
`--dryrun --quiet --include --exclude --acl --follow-symlinks --no-follow-symlinks --no-guess-mime-type --sse --sse-c --sse-c-key --sse-kms-key-id --sse-c-copy-source --sse-c-copy-source-key --storage-class --grants --website-redirect --content-type --cache-control --content-disposition --content-encoding --content-language --expires --source-region --only-show-errors --no-progress --page-size --ignore-glacier-warnings --force-glacier-transfer --request-payer`

**전역 옵션**:
`--debug --endpoint-url --no-verify-ssl --no-paginate --output --query --profile --region --version --color --no-sign-request --ca-bundle --cli-read-timeout --cli-connect-timeout`

- `--output`, `--query`, `--color`, `--no-paginate`는 받아서 값만 검증한다. AWS CLI에서도 s3 명령에는 효과가 없다.
- 1.31.13에 **없는** 플래그는 구현하지 않는다: `--checksum-algorithm`, `--checksum-mode`, `--copy-props`, `--validate-same-s3-paths`, `--no-overwrite`, `ls --bucket-name-prefix/--bucket-region`.
- 플래그 누락을 막기 위해, 기준 이미지의 실제 CLI에서 인자 테이블을 JSON으로 뽑아 gs3의 플래그 테이블과 같은지 테스트한다.
- 각 플래그의 타입·선택지·기본값과 파싱 규칙은 부록 A에 있다.

### 1.3 반드시 똑같이 맞추는 것

- **`s3 ls` 출력**: 스크립트가 awk/sed/wc로 파싱한다. `PRE` 줄, 객체 줄의 열 폭, 로컬 시간대 날짜, `--summarize` 꼬리말까지 바이트 단위로 맞춘다.
- **종료 코드**: 0 성공, 1 전송 실패 또는 `ls`에서 일치 항목 없음, 2 경고만 있음 또는 인자 오류, 255 그 밖의 오류. 스크립트가 `ls`의 종료 코드로 존재 여부를 확인한다.
- **결과 줄**: `upload: X to Y`, `download:`, `copy:`, `move:`, `delete:`, `(dryrun) …`은 표준 출력으로, 실패·경고·`fatal error:`는 표준 오류로 낸다. 오류 문구는 botocore 형식(`An error occurred (코드) when calling the 오퍼레이션 operation: 메시지`)을 따른다.
- **`cp s3://… -`**: 객체 바이트만 표준 출력에 쓴다.
- **필터**: `--include`/`--exclude`는 명령줄에 나온 순서대로 평가하고 마지막에 일치한 규칙이 이긴다.
- **sync 판정**: 부록 A.6의 규칙. 업로드·복사는 원본이 새로울 때, 다운로드는 로컬이 새로울 때 전송한다.
- **다운로드**: 임시 파일에 쓴 뒤 이름을 바꾸고 mtime을 객체의 LastModified(초 단위)로 맞춘다. sync 판정이 여기에 의존한다.
- **botocore 1.33.13 동작**
  - 사용자 지정 엔드포인트에서는 `addressing_style = virtual`이 아니면 path-style을 쓴다. Go SDK 기본값은 반대다.
  - CRC32 기본 체크섬을 보내지 않는다. Go SDK에서는 요청 체크섬 계산과 응답 검증을 둘 다 `WhenRequired`로 둔다. 두 NCP 서비스 모두 이 설정이 필요하다.
  - `PutObject`, `UploadPart`, `DeleteObjects`에 `Content-MD5`를 붙이고, HTTPS에서는 페이로드 서명을 생략한다.
  - 엔드포인트는 `--endpoint-url` → `AWS_ENDPOINT_URL_S3` → `AWS_ENDPOINT_URL` → 설정 파일 순으로 찾는다.
  - 리전은 `--region` → `AWS_DEFAULT_REGION` → 프로필 순으로 찾는다. `AWS_REGION`은 읽지 않는다.
  - 프로필은 `--profile` → `AWS_DEFAULT_PROFILE` → `AWS_PROFILE` 순이다.
  - 읽기·연결 타임아웃 기본값은 각각 60초다.

### 1.4 의도적으로 다르게 하는 것

| 항목 | AWS CLI 1.31.13 | gs3 |
|---|---|---|
| 동시 요청·청크 기본값 | 10개, 8MiB | 3.6절의 고성능 기본값. `GS3_AWS_DEFAULTS=1`이면 AWS CLI 값 |
| 재시도 | 최대 5회 시도 | 최대 8회 시도. `AWS_MAX_ATTEMPTS`를 따른다 |
| 진행률 줄 갱신 | 진행 이벤트마다 | 같은 형식, 초당 약 5회 |
| `rm --recursive`, `sync --delete` | 키마다 `DeleteObject` | 1,000개 묶음 `DeleteObjects`, 실패 시 키별 삭제로 폴백. 출력 줄은 동일 |
| 대용량 S3→S3 복사의 메타데이터 | 멀티파트 복사에서 사용자 메타데이터 유실 | `HeadObject`로 읽어 유지 |
| `Expect: 100-continue` | 파일 본문에 항상 붙임 | 붙이지 않음 (서버가 무시하면 PUT마다 1초 지연) |
| 심볼릭 링크 순환 | 감지 없음 | 감지해서 경고 후 건너뜀 |
| `presign` 서명 | AWS의 알려진 리전에서는 SigV2 | 항상 SigV4. NCP 리전은 AWS CLI도 SigV4라 결과가 같다 |
| MIME 추측 | Python `mimetypes` (시스템 파일에 따라 달라짐) | CPython 3.11 기본 테이블을 내장 |
| `help` 출력 | man 페이지 렌더링 | 명령별 간단한 도움말 |
| `--debug` 로그 | botocore 로그 | SDK 요청·응답·재시도 로그 |
| `--version` | `aws-cli/1.31.13 Python/…` | `gs3/<버전> (aws-cli/1.31.13 compatible) Go/…` |

## 2. 저장소 구조

```
gs3/
├── cmd/gs3/main.go          진입점: os.Exit(cli.Main(...)). 정리 작업이 끝난 뒤에만 종료
├── internal/
│   ├── cli/                 argparse 호환 파서, 플래그 테이블, usage·오류, 종료 코드 매핑, paramfile
│   ├── command/             ls cp mv rm sync mb rb presign website (명령당 파일 하나)
│   ├── awscfg/              botocore 순서의 환경변수·설정 파일 해석, `s3 =` INI 파서, 크기·속도 값 파싱
│   ├── s3client/            클라이언트 생성(엔드포인트, path-style, 체크섬·서명 미들웨어, 재시도, 전송 계층, DNS 캐시)
│   ├── tune/                cgroup CPU·메모리 감지, 기본값 계산
│   ├── core/                공용 타입: Entry, Task, Op
│   ├── s3path/              경로 분류, 대상 키·비교 키 계산, 표시용 상대 경로
│   ├── filter/              --include/--exclude, Python fnmatch 호환 매처
│   ├── listing/             Lister 인터페이스, S3 순차·병렬 리스터, 로컬 워커, 순서 보존 합류
│   ├── syncer/              병합 비교기와 sync 전략
│   ├── transfer/            전송 엔진: 제한기, 배분기, 단일 요청 작업, 멀티파트, 스트림, 묶음 삭제, 정리 레지스트리
│   ├── output/              결과 타입, 출력기(표준 출력·오류의 유일한 소유자), 진행률, 크기 표기
│   └── mimetype/            Python mimetypes 호환 테이블
├── tools/probe/             NCP 기능 점검 프로그램
├── test/integration/        MinIO 통합 테스트
├── test/compat/             awscli 1.31.13 대비 차등 테스트 하니스와 시나리오
├── bench/                   데이터셋 생성기, 벤치마크 스크립트, 결과
├── docs/compat-spec.md      1.31.13 동작 스펙 (부록 A를 옮겨 적고 계속 보강)
├── deploy/                  Dockerfile, docker-compose.yml (MinIO + awscli 1.31.13 기준 이미지)
└── Makefile, go.mod, .golangci.yml, README.md
```

- **핵심 파일**: `internal/transfer/engine.go`, `internal/listing/s3parallel.go`, `internal/syncer/join.go`, `internal/s3client/client.go`, `internal/cli/parser.go`.
- Go 1.27(SDK 최소 요구는 1.24), `CGO_ENABLED=0`. 모듈 경로는 `gs3`로 시작하고 원격 저장소가 정해지면 바꾼다.
- 의존성: `aws-sdk-go-v2` v1.48.0, `config` v1.33.18, `service/s3` v1.114.10, `smithy-go`, `golang.org/x/time/rate`(대역폭 제한), `go-jmespath`(`--query` 검증).
- SDK의 `feature/s3/manager`(지원 종료 예정)와 `transfermanager`(v0.x)는 쓰지 않는다. 멀티파트 복사와 묶음 삭제가 없고, 호출마다 동시성 풀이 따로라 전체 요청 수를 하나로 묶을 수 없다.
- CLI 파서는 직접 작성한다. cobra/pflag는 argparse의 `nargs`(`--grants a=b c=d`), 긴 플래그 접두어 축약, 전역 옵션 우선 매칭을 지원하지 않는다.
- `s3client`는 좁은 `API` 인터페이스를 내보내 테스트에서 가짜 구현으로 바꿀 수 있게 한다.

## 3. 아키텍처

### 3.1 실행 흐름

```
원본 리스터 ─ 페이지 ─> 필터 ─┐
                              ├─> 비교기 ─ 작업 큐 ─> 배분기 ─┬─ 작은 작업: 토큰 1개 → 요청 1개
대상 리스터 ─ 페이지 ─> 필터 ─┘   (sync만)                    ├─ 대용량: 조정 goroutine → 파트마다 토큰 1개
                                                              └─ 삭제: 1,000개 묶음 → 토큰 1개
모든 작업 ─ 결과 채널 ─> 출력기 ─> 표준 출력 / 표준 오류
```

- 모든 단계는 goroutine이고 크기가 정해진 채널로 이어진다. 전체 목록을 메모리에 올리지 않는다.
- 뒤가 밀리면 앞이 멈춘다. 표준 출력이 막히면 출력기, 작업, 배분기, 비교기, 리스터 순으로 멈춰 페이지 조회도 멈춘다.
- 리스터는 항목을 최대 1,000개 묶음으로 넘긴다.
- sync에서는 원본과 대상 목록 조회를 동시에 돌린다. AWS CLI는 한 스레드에서 번갈아 읽는다.
- 판정이 나는 대로 전송을 시작한다. 목록 조회가 끝나기를 기다리지 않는다.

### 3.2 동시성 모델

- **제한기 3개**
  - 전송 제한기(N): HTTP 전송 요청 하나가 토큰 하나를 쓴다. 작은 파일, 대용량 파일의 파트, 묶음 삭제가 같이 쓴다.
  - 목록 제한기(W): 목록 조회 전용. 목록 조회가 전송 뒤에 줄 서면 파이프라인 전체가 굶는다.
  - 정리 제한기(8): 취소 뒤에도 멀티파트 중단과 임시 파일 삭제가 돌아야 한다.
- 제한기는 뮤텍스와 선입선출 대기열로 직접 만든다. 실행 중에 한도를 바꿀 수 있어야 한다.
- **작은 파일**: 작업 하나가 토큰 하나를 잡고 요청 하나로 끝낸다. 재시도 대기 중에도 토큰을 쥐고 있어서, 재시도가 몰리면 실제 동시성이 저절로 내려간다.
- **대용량 파일**: 파일마다 조정 goroutine 하나가 `생성 → 파트 → 완료`를 진행한다.
  - 조정 goroutine은 기다리는 동안 토큰을 쥐지 않는다. 파트마다 토큰을 얻어 goroutine을 띄운다.
  - 첫 파트 실패가 그 파일의 컨텍스트를 취소한다. 실패하면 정리 제한기로 멀티파트 업로드를 중단한다.
- **대용량 파일 입장 제한**: 용량 2N인 가중 세마포어에 파일마다 `min(파트 수, N)`만큼 넣는다. 큰 파일 하나는 예산의 절반을 쓰고, 파트 3개짜리 파일은 수십 개가 함께 열린다.
- **교착 방지**: 획득 순서는 항상 작업 큐 → 입장 가중치 → 토큰이다. 토큰을 쥔 채 토큰을 기다리는 goroutine이 없고, 모든 대기는 취소를 함께 기다린다.
- **적응형 동시성**: 429나 503을 받으면 한도를 `max(4, 한도/2)`로 내리고 5초 유지한 뒤, 조용한 1초마다 1씩 N까지 올린다. 목록 제한기에도 같은 비율로 적용한다. 기본으로 켠다.

### 3.3 전송 방식

| 종류 | 방식 |
|---|---|
| 작은 업로드 | 파일을 본문으로 `PutObject`. `Content-MD5`는 본문을 먼저 한 번 읽어 계산 |
| 대용량 업로드 | 파트마다 `io.SectionReader`로 파일 구간을 직접 읽는다. 파트 버퍼를 잡지 않고 재시도 때 되감을 수 있다 |
| 작은 다운로드 | `GetObject` 한 번으로 임시 파일에 기록 |
| 대용량 다운로드 | 범위 GET을 오름차순으로 배정해 `WriteAt`으로 기록. 범위마다 `If-Match`로 도중 변경을 감지. 파일 크기를 미리 잡지 않는다 |
| S3→S3 복사 | 임계값 미만은 `CopyObject`, 이상은 `UploadPartCopy` 병렬(파트 64MiB). 미지원 응답이면 그 엔드포인트에서는 `CopyObject`(5GiB 이하) 또는 GET→`UploadPart` 중계로 폴백 |
| 삭제 | 1,000개 또는 50ms 유휴 시 묶음 `DeleteObjects`. `mv`의 원본 삭제는 해당 파일 성공 직후 개별 삭제 |
| 표준 출력으로 스트리밍 | 버퍼 4개(각 8MiB)를 파트 순서대로 나눠 주고 순서대로 내보낸다 |
| 표준 입력에서 업로드 | 빈 버퍼를 얻어 채운 뒤 `UploadPart`. 한 파트보다 짧으면 `PutObject`. 파트 크기는 `--expected-size`로 정한다 |

- 파트 크기는 `max(청크 크기, ceil(파일 크기 / 최대 파트 수))`이고 5MiB~5GiB로 제한한다. 최대 파트 수는 10,000이고 `GS3_MAX_PARTS`로 낮출 수 있다.
- 다운로드 후 처리: 임시 파일(`<이름>.<8자리 hex>`) → 이름 변경 → mtime 설정.
- 응답 본문을 읽다 끊기면 SDK는 재시도하지 않는다. 엔진이 현재 오프셋부터 `If-Match`로 이어받는다.
- `CompleteMultipartUpload` 응답을 잃고 재시도해 `NoSuchUpload`가 오면 `HeadObject`로 확인해 성공으로 처리한다.
- `max_bandwidth`는 전송 전체가 공유하는 토큰 버킷으로 제한한다.

### 3.4 목록 조회

- **S3**: `ListObjectsV2` 페이지네이터. 1.31.13도 항상 V2를 쓴다. `GS3_LIST_OBJECTS_V1=1`로 V1을 쓸 수 있다.
- **순서 보존 병렬 조회** (M7에서 추가, 순차 리스터와 같은 인터페이스)
  1. 탐색: 구분자 없이 한 페이지를 받는다. 잘리지 않았으면 그대로 끝. 대부분의 호출이 여기서 끝난다.
  2. 판단: 이 페이지에 나온 바로 아래 하위 접두어 수 `c`를 센다. `1 ≤ c ≤ W/2`일 때만 나눈다. 하위 접두어가 적고 크다는 뜻이다. 아니면 이어받기 토큰으로 순차 조회를 계속한다.
  3. 분할: 구분자 `/`로 그 단계를 조회해 직접 객체와 하위 접두어를 바이트 순으로 합친다. 하위 접두어마다 1부터 다시 적용한다(깊이 6까지).
  4. 합류: 접두어 순서대로 이어 붙인다. 뒤쪽 접두어는 2페이지까지만 미리 받고 기다린다.
  5. 보호 장치: 하위 탐색 16회의 평균이 64키 미만이면 분할을 멈추고 나머지를 `StartAfter`로 순차 조회한다.
  6. `GS3_PARALLEL_LIST=0`으로 끈다.
- 순서가 겉으로 보이는 경우(sync, `ls --recursive`, `--dryrun`)에만 순서를 지킨다. `rm --recursive`와 dryrun이 아닌 `cp/mv --recursive`는 같은 분할기를 순서 없이 합류시킨다.
- **로컬**: 디렉터리마다 항목을 읽고, 하위 디렉터리 이름에 `/`를 붙인 뒤 바이트 순으로 정렬한다. 이렇게 해야 S3 키 순서와 맞는다. 하위 디렉터리는 4개까지 병렬로 읽고 같은 방식으로 순서대로 합류시킨다.
- 기본은 심볼릭 링크를 따라간다. 특수 파일·읽을 수 없는 파일은 경고 후 건너뛴다(부록 A.7).
- **`Lister` 계약**: 끝(`io.EOF`)은 목록을 끝까지 받았을 때만 낸다. 오류나 취소는 절대 끝으로 보이지 않는다. 잘린 원본 목록이 "원본에 없음"으로 읽히면 `--delete`가 대상을 지우기 때문이다. 각 스트림의 키 순서도 실행 중에 검사해 어긋나면 중단한다.

### 3.5 클라이언트 설정

- 자격 증명은 `config.LoadDefaultConfig`의 체인(환경변수, 공유 파일, assume-role, SSO, `credential_process`, 컨테이너·IMDS)을 쓴다.
- 프로필·리전·엔드포인트는 gs3가 1.3절의 botocore 순서로 직접 정해 덮어쓴다. SDK는 `AWS_REGION`을 읽기 때문에 그대로 두면 AWS CLI와 달라진다.
- 사용자 지정 엔드포인트에 리전이 없으면 서명 리전을 `us-east-1`로 둔다(botocore와 동일).
- SDK가 읽지 않는 `s3 =` 중첩 블록은 자체 INI 파서로 읽는다: `max_concurrent_requests`, `max_queue_size`, `multipart_threshold`, `multipart_chunksize`, `max_bandwidth`, `addressing_style`, `payload_signing_enabled`, `use_accelerate_endpoint`, `use_dualstack_endpoint`.
- **HTTP 전송 계층**: 프로세스에 `http.Transport` 하나.
  - HTTP/1.1 고정. 호스트당 유휴 연결 수는 `N + W + 8`. SDK 기본값 10이면 연결을 계속 새로 맺는다.
  - 읽기·쓰기 버퍼 64KiB, TLS 세션 캐시.
  - DNS 캐시: 다이얼 단계에서 같은 호스트 조회를 하나로 합치고 30초 캐시하며 A 레코드를 돌려 쓴다. Kubernetes에서는 시작할 때 N개 연결이 동시에 DNS를 조회한다.
  - `--cli-connect-timeout`은 다이얼 타임아웃, `--cli-read-timeout`은 읽기·쓰기마다 갱신하는 연결 유휴 타임아웃으로 구현한다. `CopyObject`와 `CompleteMultipartUpload`는 응답 대기를 최소 300초로 둔다.
  - `--no-verify-ssl`, `--ca-bundle`/`AWS_CA_BUNDLE` 반영.
- **재시도**: 표준 재시도기, 최대 8회, 전체 지터, 대기 상한 20초. SDK의 클라이언트 측 재시도 토큰 버킷은 끈다. 높은 동시성에서 스로틀링이 오면 버킷이 비어 요청이 즉시 실패하기 때문이다.
- **무결성과 서명**
  - 업로드에 `Content-MD5`를 붙인다(기본 켬).
  - HTTPS에서는 서명 없는 페이로드 미들웨어를 명시적으로 넣어 SHA256 계산을 피한다. HTTP에서는 SHA256 서명을 쓴다. M0에서 실제 요청 헤더로 확인한다.
  - `DeleteObjects`와 `PutBucketWebsite`에는 자체 미들웨어로 `Content-MD5`를 넣는다. SDK는 CRC32만 보낸다.
- **정적 바이너리 보완**: `time/tzdata`를 내장한다(`ls`가 로컬 시간을 출력). 시스템 CA가 없을 때 쓸 예비 루트 인증서를 포함한다.

### 3.6 기본값

| 설정 | AWS CLI | gs3 초기값 | 근거 |
|---|---|---|---|
| 전송 동시성 N (`max_concurrent_requests`) | 10 | `clamp(32 × CPU 제한, 16, 128)`. 메모리 256Mi 이하면 최대 32, 512Mi 이하면 최대 64 | 작은 파일은 요청 대기 시간이 지배적이라 연결 수가 처리량을 정한다 |
| 목록 동시성 W | 1 | `clamp(N/4, 4, 8)` | 미리 읽기 용도 |
| `multipart_threshold` | 8MiB | 32MiB | 요청 수를 줄인다 |
| `multipart_chunksize` | 8MiB | 16MiB | 파트 버퍼가 없어 청크를 키워도 메모리가 늘지 않는다 |
| `max_queue_size` | 1000 | 2000 | 목록 2페이지 분량 |
| 스트리밍 창 | 6 × 8MiB | 4 × 8MiB, 512Mi 이하면 2 × 8MiB | 프로그램에서 유일한 파트 버퍼 |
| 다운로드 버퍼 | 256KiB | GET마다 256KiB, 256Mi 이하면 64KiB | |

- CPU 제한은 cgroup의 `cpu.max`에서 소수 값으로 직접 읽는다. 0.1 CPU면 16, 1 CPU면 32, 4 CPU면 128이 된다.
- cgroup 메모리 제한이 있고 `GOMEMLIMIT`이 없으면 `clamp(제한의 50%, 96MiB, 2GiB)`로 둔다.
- 예상 최대 메모리(추정치, 페이지 캐시 제외): `30MiB + N × (0.2MiB + 다운로드 버퍼) + W × 3MiB + 스트리밍 창`. N=16에서 약 46MiB, N=64에서 약 83MiB, N=128에서 약 113MiB.
- 우선순위: `GS3_*` 환경변수 > `s3 =` 블록의 명시값 > 위 기본값.
- 이 값들은 M7 벤치마크로 확정한다. 표의 숫자는 출발점이다.

**조정용 환경변수** (플래그는 AWS CLI와 동일하게 유지하고 확장은 환경변수로만 둔다):
`GS3_MAX_CONCURRENT_REQUESTS`, `GS3_MULTIPART_THRESHOLD`, `GS3_MULTIPART_CHUNKSIZE`, `GS3_MAX_QUEUE_SIZE`, `GS3_MAX_BANDWIDTH`, `GS3_MAX_PARTS`, `GS3_AWS_DEFAULTS`, `GS3_CONTENT_MD5`, `GS3_BATCH_DELETE`, `GS3_PARALLEL_LIST`, `GS3_LIST_OBJECTS_V1`, `GS3_ADAPTIVE`, `GS3_PPROF`

### 3.7 출력과 실패 처리

- 출력은 goroutine 하나가 맡는다. 진행 바이트는 원자적 카운터로 세고 출력기가 주기적으로 읽는다.
- 작업은 결과를 보낸 뒤에 토큰을 놓는다.

| 분류 | 예 | 처리 |
|---|---|---|
| 파일 실패 | 재시도 후에도 요청 실패, 열기·이름 변경 오류, 업로드 중 파일이 줄어듦 | `<종류> failed: …`을 표준 오류에 내고 나머지는 계속. 종료 코드 1 |
| 경고 | 특수·읽기 불가·없는 파일, 글레이셔 객체, 상위 디렉터리를 가리키는 키 | `warning: …`. 실패가 없으면 종료 코드 2 |
| 치명적 오류 | 목록 조회 오류, 없는 버킷, 자격 증명 | 전체 취소, `fatal error: …`, 종료 코드 1. 실행 전 검증 오류는 255 |
| 신호 | SIGINT, SIGTERM | 취소 후 정리. 두 번째 신호는 즉시 종료 |

- 종료 코드 결정: 치명적 오류 → 실패가 있으면 1 → 경고가 있으면 2 → 0.
- 정리 레지스트리가 임시 파일 경로와 멀티파트 업로드 ID를 갖고 있다. 취소되면 10초 한도의 별도 컨텍스트로 임시 파일을 지우고 업로드를 중단한다.
- SIGKILL로 죽으면 끝나지 않은 멀티파트 업로드가 남는다. 버킷 수명 주기 규칙으로 정리하도록 README에 적는다.

## 4. NCP 두 서비스 대응

| 항목 | Object Storage | Ncloud Storage | gs3 대응 |
|---|---|---|---|
| 엔드포인트 | `kr.object[.private].ncloudstorage.com` 등, 리전 `kr-standard` | `kr[-private].ncloudstorage.com` | `--endpoint-url`/`AWS_ENDPOINT_URL` 그대로 사용 |
| 주소 방식 | 문서 예시가 path-style | 문서 예시가 virtual-hosted | 기본 path-style(botocore와 동일), `addressing_style`로 전환 |
| `ListObjectsV2` | 문서가 서로 다름 | 지원 | 1.31.13이 V2만 쓰고 현재 동작하므로 V2 기본, V1 스위치 제공 |
| `UploadPartCopy` | 문서에 없음 | 지원 | 미지원 응답 시 자동 폴백(3.3) |
| 최대 파트 수 | 미확인 | 문서에 10,000과 1,000이 함께 있음 | 기본 10,000, `GS3_MAX_PARTS` |
| `DeleteObjects` | 문서 설명이 불명확 | 지원 | `Content-MD5` 미들웨어, 실패 시 키별 삭제로 폴백 |
| 스로틀링 | 공개 수치 없음 | 429 `SlowDown` | 적응형 동시성 |
| `website` | 미지원 | API 존재 여부 미확인 | 명령은 구현하고 서버 오류를 그대로 전달 |

- "미확인"과 "문서가 서로 다름" 항목은 M0의 기능 점검으로 확정하고 `docs/compat-spec.md`에 적는다.
- NCP 공식 가이드가 Object Storage에 AWS CLI 1.15.85를 권장하고, 최신 CLI·SDK에서 Access Denied가 난다는 FAQ가 있다. 원인은 새 체크섬 헤더로 추정되며 1.3절의 `WhenRequired` 설정이 이를 피한다.

## 5. 마일스톤

각 단계는 실행해서 확인할 수 있는 상태로 끝난다.

| 단계 | 내용 | 완료 기준 |
|---|---|---|
| **M0 준비와 기능 점검** | `brew install go`, `git init`, 모듈·Makefile·lint. compose로 MinIO와 기준 이미지(`python:3.11-slim` + `awscli==1.31.13`). 기준 CLI의 인자 테이블을 JSON으로 덤프. 부록 A를 `docs/compat-spec.md`로 이관. `tools/probe`로 두 NCP 서비스 점검 | `gs3 --version` 빌드. 점검 결과표가 문서에 기록됨 |
| **M1 파서와 단순 명령** | `cli`, `awscfg`, `s3client`, `output` 기초. `ls`, `mb`, `rb`, `presign`, `website` | 파서 표 테스트 통과. 플래그 테이블이 덤프와 일치. `ls` 출력·종료 코드가 기준과 일치 |
| **M2 단일 요청 엔진** | 제한기, 단일 파일 `cp`(업로드·다운로드·복사)와 `rm`, 출력기, 종료 코드, 임시 파일 → 이름 변경 → mtime | MinIO에서 왕복 후 내용 해시·mtime 일치 |
| **M3 재귀 작업** | 순차 리스터(로컬·S3), 필터, `--recursive` `cp`/`mv`/`rm`, 묶음 삭제, dryrun, 경고, 요청 파라미터 매핑, `rb --force` | 기준과 전송 집합·출력 줄 일치 |
| **M4 sync** | 병합 비교기, 전략 4종, 원본·대상 동시 조회 | 같은 입력에서 gs3와 기준이 고른 전송·삭제 집합이 일치 |
| **M5 대용량과 스트림** | 멀티파트 업로드, 범위 다운로드, 멀티파트 복사와 폴백, 정리 레지스트리, 신호 처리, 표준 입출력 스트리밍 | 실패·SIGINT 뒤 남은 멀티파트 업로드와 임시 파일이 없음 |
| **M6 견고성** | 본문 읽기 재개, 연결 유휴 타임아웃, 적응형 동시성, Complete 중복 처리, `max_bandwidth`, 장애 주입 테스트 | 503·연결 끊김·잘린 본문을 주입해도 결과가 정확함 |
| **M7 성능** | 컨테이너 인지 기본값, 병렬 조회(S3·로컬), 버퍼·전송 계층 조정, PGO 빌드, 벤치마크로 기본값 확정, `Content-MD5` 비용 측정 | 6절의 성능 목표 달성. 차등 테스트 재통과 |
| **M8 마무리와 배포** | 전체 플래그 차등 테스트, NCP 장시간 테스트, 실제 워크플로를 `aws`와 gs3로 나란히 돌려 비교, linux/amd64·linux/arm64·darwin/arm64 정적 빌드, Docker 이미지, README | 워크플로 템플릿 하나가 gs3로 끝까지 통과. 남은 차이는 1.4절 표에 있는 것뿐 |

**M0 기능 점검 항목** (두 서비스 각각, `tools/probe`는 SDK를 직접 호출하는 작은 Go 프로그램):
- 목록이 바이트 순서로 오는지, `ListObjectsV2`와 `StartAfter`가 되는지
- path-style과 virtual-hosted 중 무엇이 되는지
- `DeleteObjects`가 `Content-MD5`로 되는지
- 서명 없는 페이로드, `Expect: 100-continue`, `If-Match` 범위 GET
- `UploadPartCopy`, 최대 파트 수, 최소 파트 크기
- `LastModified` 정밀도, 429/503 응답 형태

자격 증명은 환경변수로만 받고 저장소에 남기지 않는다.

## 6. 검증

**단위 테스트** (`go test -race ./...`)
- 파서: 부록 A.3의 규칙마다 표 테스트. gs3 플래그 테이블이 기준 CLI 덤프와 같은지 검사.
- fnmatch와 표시용 상대 경로: Python으로 만든 테스트 벡터.
- 정렬: 로컬 워커의 순서가 전체 키를 바이트 정렬한 것과 같은지 속성 테스트.
- sync 판정: 작업 종류 × 크기 × mtime × 플래그 조합표.
- 병렬 리스터가 무작위 트리에서 순차 리스터와 같은 결과를 내는지.
- 전송 엔진: 실패하는 가짜 API로 재시도, 부분 실패, 취소 시 정리, 토큰 상한을 확인. goroutine 누수 검사, N=1 포함.
- 리스터가 도중에 실패할 때 `--delete`가 아무것도 지우지 않는지.

**통합 테스트** (`make integration`)
- MinIO 컨테이너에서 9개 명령을 끝까지 실행하고 내용 해시를 확인.
- 장애 주입 프록시: 503, 연결 리셋, 잘린 본문, 200 응답 안의 오류.
- `--cpus 0.1 --memory 256m`에서 파일 20만 개와 8GiB 객체 1개.

**차등 테스트** (`make compat`)
- 같은 인자를 `aws`(1.31.13 컨테이너)와 `gs3`에 주고, 같은 초기 상태의 버킷과 로컬 트리에서 비교한다.
- 비교 대상: 종료 코드, 표준 출력, 표준 오류의 분류, 버킷 상태(키, 크기, 해시, Content-Type, 메타데이터), 로컬 상태(경로, 크기, 해시, 초 단위 mtime).
- 정규화: `\r`과 `\n`으로 나눠 `Completed …` 조각과 뒤 공백을 제거. 전송 결과 줄은 정렬 후 비교하고 `ls`와 `(dryrun)`은 순서 그대로 비교. 임시 파일 접미사와 요청 ID는 가린다. presign URL은 구조로 비교.
- `ls`는 `TZ=UTC`와 `TZ=Asia/Seoul` 둘 다에서 돌린다.
- 시나리오: 워크플로에서 실제로 쓰는 호출 형태를 먼저 넣고, 플래그별 시나리오를 더한다.

**벤치마크** (`make bench`)

| 구분 | 내용 |
|---|---|
| 데이터셋 | 10만 × 4KiB, 1만 × 1MiB, 500 × 40MiB, 1 × 20GiB, 실제와 비슷한 혼합, 키 100만 개(평면 / 1000×1000) |
| 작업 | 방향별 sync(처음, 변경 없음, 1% 변경), S3→S3 sync, `rm --recursive`, 대용량 `cp`(양방향, 표준 출력), 파일마다 프로세스를 띄우는 루프 1,000회, `ls --recursive` |
| 비교 대상 | aws-cli 1.31.13(기본값, 동시 요청 64), s5cmd, gs3 |
| 환경 | 로컬 MinIO, 지연·대역폭 제한을 건 MinIO, 파드에서 실제 NCP |
| 제한 | `--cpus` {0.1, 1, 4} × `--memory` {256m, 1g, 8g} |
| 측정 | 소요 시간, MB/s, 객체/s, 요청 수, CPU 시간, 재시도 수, 최대 RSS, cgroup `memory.peak` |

- **성능 목표** (측정 전 가설): 작은 파일 업로드와 변경 없는 sync는 aws-cli 기본값 대비 5배 이상, 대용량 파일은 2배 이상, 기본값에서 최대 RSS 150MiB 이하.

## 7. 위험과 미확정 사항

| 위험 | 대응 |
|---|---|
| 목록 조회 오류가 끝으로 읽혀 `--delete`가 대상을 지움 | `Lister` 계약(3.4), 도중 실패 테스트 |
| NCP 두 서비스의 API 차이가 문서로 확정되지 않음 | M0 기능 점검, 기능별 자동 폴백과 `GS3_*` 스위치 |
| 높은 기본 동시성이 스로틀링을 부름 | 적응형 동시성, 재시도 대기 중 토큰 유지, M7에서 실제 엔드포인트로 기본값 조정 |
| `Content-MD5` 계산이 CPU 100m 컨테이너에서 업로드를 느리게 함 | M7에서 비용을 재고 결과를 보고. `GS3_CONTENT_MD5=0` 제공 |
| argparse 세부 동작(위치 인자 인접 규칙 등)이 Python 버전에 따라 다를 수 있음 | 차등 테스트로 3.11 기준 동작에 맞춤 |
| 백엔드가 바이트 순서로 목록을 주지 않음 | M0에서 까다로운 키로 확인, 실행 중 순서 검사 |
| mtime 정밀도 차이로 sync가 불필요하게 재전송 | AWS CLI와 같이 초 단위로 내려 적고 같은 비교식 사용 |
| tmpfs emptyDir와 페이지 캐시가 메모리 제한에 잡힘 | 오름차순 쓰기, `memory.peak` 측정 |
| MIME 추측이 기준 환경의 시스템 파일에 좌우됨 | 내장 테이블로 고정하고 차이를 문서화 |
| 진행 중 SIGINT의 종료 코드가 소스로 확정되지 않음 | 차등 테스트로 확인 후 맞춤 |

**범위 밖, 후속 후보**
- 같은 워크플로가 `s3api head-object`, `list-objects-v2`, `head-bucket`, `delete-bucket`도 호출한다. 이 스텝들은 gs3만으로 `aws`를 완전히 대체할 수 없으므로, 필요하면 이 네 명령을 후속으로 추가한다.
- 파일마다 `aws s3 cp`를 띄우는 루프는 gs3로도 프로세스마다 TLS 연결을 새로 맺는다. `cp --recursive` 한 번으로 고치는 편이 훨씬 빠르다.
- AWS S3의 버킷 리전 자동 리다이렉트, S3 Express, Windows 지원.

---

## 부록 A. AWS CLI 1.31.13 동작 스펙 (소스 확인)

기준: aws-cli 1.31.13, botocore 1.33.13, s3transfer 0.8.2. M0에서 `docs/compat-spec.md`로 옮긴다.

### A.1 플래그 타입

| 플래그 | 타입 | 선택지 / 기본값 |
|---|---|---|
| `--include`, `--exclude` | 값 1개, 반복 가능, 한 목록에 순서대로 쌓임 | |
| `--acl` | 문자열 | private, public-read, public-read-write, authenticated-read, aws-exec-read, bucket-owner-read, bucket-owner-full-control, log-delivery-write |
| `--follow-symlinks` / `--no-follow-symlinks` | 불리언 쌍 | 기본 따라감, 마지막에 준 것이 이김 |
| `--sse` | 값 0~1개 | AES256(값 없을 때), aws:kms |
| `--sse-c`, `--sse-c-copy-source` | 값 0~1개 | AES256 |
| `--request-payer` | 값 0~1개 | requester |
| `--storage-class` | 문자열 | STANDARD, REDUCED_REDUNDANCY, STANDARD_IA, ONEZONE_IA, INTELLIGENT_TIERING, GLACIER, DEEP_ARCHIVE, GLACIER_IR |
| `--grants` | 값 1개 이상 | `권한=대상`, 권한은 read, readacl, writeacl, full |
| `--metadata` | 맵 | `k=v,k2=v2` 또는 `{`로 시작하는 JSON |
| `--metadata-directive` | 문자열 | COPY, REPLACE |
| `--page-size`, `--expires-in` | 정수 | `--expires-in` 기본 3600 |
| `--expected-size`, `--expires` | 문자열 | `--expires`는 그대로 전달 |
| `--cli-read-timeout`, `--cli-connect-timeout` | 정수 | 기본 60, 0은 무제한 |
| `--output` | 문자열 | json, text, table |
| `--color` | 문자열 | on, off, auto |
| 나머지 | 불리언 또는 문자열 | |

### A.2 경로 규칙

- `s3://`로 시작하면 S3, 아니면 로컬. 허용 조합: cp/mv/sync는 로컬→S3, S3→로컬, S3→S3. rm/mb/rb는 S3만. 그 밖은 `usage: aws s3 <cmd> <USAGE>\nError: Invalid argument type`, 종료 코드 255.
- `ls`, `presign`, `website`는 `s3://` 없이도 받는다.
- `s3://bucket`은 `s3://bucket/`으로 본다. `sync`, `mb`, `rb`는 항상 디렉터리 동작.
- 대상 경로: 디렉터리 동작이면 원본 루트 기준 상대 경로를 붙인다. 아니면 대상이 `/`로 끝나거나 기존 디렉터리일 때만 원본 파일명을 붙인다.
  - `cp f s3://b/p/` → `p/f`, `cp f s3://b/p` → `p`, `cp s3://b/k .` → `./k`, `cp s3://b/k newdir/` → `newdir/k`
- 재귀가 아닌 S3 원본은 `HeadObject`로 확인한다. 삭제는 생략한다. 404면 `Key "<key>" does not exist`.
- `s3://b/`를 재귀 없이 주면 아무것도 하지 않고 종료 코드 0.
- `mv`에서 원본과 대상이 같으면 `Cannot mv a file onto itself: '%s' - '%s'`, 255.
- 로컬 원본이 없으면 `The user-provided path %s does not exist.`, 255.
- S3→로컬 디렉터리 동작은 `--dryrun`에서도 대상 디렉터리를 만든다.
- `-`는 재귀가 아닌 `cp`에서만 허용: `Streaming currently is only compatible with non-recursive cp commands`, 255. 스트리밍이면 `--only-show-errors`가 강제된다.
- SSE-C 검증 문구(원문 오타 "It" 포함): `It --sse-c is specified, --sse-c-key must be specified as well.` 등, 255.
- `rb`에 키가 있으면 `Please specify a valid bucket name only. E.g. s3://%s`, 255. `--force`는 `rm --recursive`를 먼저 돌리고 0이 아니면 `remove_bucket failed: Unable to delete all objects in the bucket, bucket will not be deleted.`, 255.
- `mb`는 리전이 `us-east-1`이 아니면 `LocationConstraint`로 보낸다.
- `ls`: 버킷이 없으면 `ListBuckets`. `--recursive`면 구분자 없이 전체 키, 아니면 구분자 `/`로 마지막 구성 요소만 표시.
- `--source-region`은 S3→S3에서 원본 클라이언트를 따로 만든다. 이 클라이언트에는 `--endpoint-url`이 적용되지 않고 환경변수·설정의 엔드포인트만 적용된다.

### A.3 파싱 규칙

- 3단계: 전역 파서가 argv 전체에서 전역 옵션을 먼저 떼고, `s3`, 하위 명령 순으로 나머지를 넘긴다.
- 긴 플래그는 유일한 접두어로 줄여 쓸 수 있다(`--recur`, `--exc`). 전역 파서가 먼저 돌기 때문에 전역 옵션과 유일하게 맞는 접두어는 전역으로 잡힌다(`--re x` → `--region x`).
- 남는 토큰은 `"\nUnknown options: a,b\n"`을 표준 오류에 쓰고 255.
- argparse 오류(위치 인자 누락, 잘못된 선택지, 값 누락)는 usage와 `aws: error: <msg>`를 표준 오류에 쓰고 2.
- 위치 인자 2개는 붙어 있어야 한다. `cp a b --recursive`와 `cp --recursive a b`는 되고 `cp a --recursive b`는 `the following arguments are required: paths`, 2.
- 값 0~1개 플래그(`--sse` 등)는 뒤의 위치 인자를 삼킨다. `--grants`는 다음 `-` 토큰 전까지 모두 삼킨다.
- paramfile: `file://`(텍스트), `fileb://`(바이트), `http(s)://`(`cli_follow_urlparam`이 true일 때 GET). 문자열 하나짜리 인자에만 적용된다. `--website-redirect`, cp/mv/sync의 경로 2개, 필터, 전역 옵션에는 적용되지 않는다. 실패 시 `Error parsing parameter '<name>': Unable to load paramfile <path>: <err>`, 255.
- `--endpoint-url`에 스킴이 없으면 `Bad value for --endpoint-url "%s": scheme is missing. Must be of the form http://<hostname>/ or https://<hostname>/`, 255.

### A.4 필터

- Python `fnmatch`: `*`는 `/`도 넘어 일치, `?`, `[seq]`, `[!seq]`. POSIX에서 대소문자 구분.
- 패턴은 루트에 붙여 전체 경로와 비교한다. 로컬 루트는 원본의 절대 경로(디렉터리 동작이 아니면 그 상위), S3 루트는 `bucket/key`.
- 같은 패턴을 대상 루트에도 붙여 대상 목록에 적용한다. 그래서 제외된 파일은 `--delete`로 지워지지 않는다.
- 파일은 포함 상태로 시작하고, 규칙을 순서대로 보며 일치할 때마다 상태를 덮어쓴다.

### A.5 요청 파라미터

- Put/Copy/CreateMultipartUpload: ACL, StorageClass, WebsiteRedirectLocation, ContentType, CacheControl, ContentDisposition, ContentEncoding, ContentLanguage, Expires, Grant*, Metadata, ServerSideEncryption, SSEKMSKeyId, SSECustomer*.
- Copy/UploadPartCopy: CopySourceSSECustomer*. Get: SSE-C, RequestPayer. Delete: RequestPayer. RequestPayer는 Head·List에도 붙는다.
- Copy에서 `--metadata`를 주고 지시자를 안 주면 `MetadataDirective=REPLACE`가 된다.
- MIME: `--content-type`이 없고 추측이 켜져 있으면 업로드는 로컬 경로로, 복사는 원본 키로 추측한다. 스트림은 추측하지 않는다.
- 5TiB 초과: `warning: File <rel> exceeds s3 upload limit of 5.0 TiB.` 경고 후 시도.

### A.6 sync 판정

- 두 목록을 비교 키(상대 경로, `/` 구분) 순으로 한 번에 병합한다.
  - 양쪽에 있음: 아래 전략으로 판정.
  - 원본에만 있음: 전송.
  - 대상에만 있음: 기본은 무시, `--delete`면 삭제.
- 기본 전략: 크기가 다르면 전송. 같으면 시간으로 판정.
  - 업로드·복사: 원본이 대상보다 **엄격히 새로우면** 전송.
  - 다운로드: 로컬이 S3보다 **엄격히 새로우면** 전송.
- `--size-only`: 크기가 다를 때만 전송.
- `--exact-timestamps`: 다운로드에서 시간이 정확히 같지 않으면 전송. 업로드·복사는 기본 규칙. `--size-only`와 함께 주면 `--exact-timestamps`가 이긴다.
- 로컬 시간은 `stat`(링크를 따라감)의 mtime, 마이크로초 유지. S3 시간은 `LastModified`.
- 대상 목록에서는 크기 0이고 `/`로 끝나는 키를 건너뛴다. 삭제 작업(`rm --recursive`)에서는 포함한다.
- S3 대상 삭제는 원본 클라이언트로 수행한다.

### A.7 경고와 건너뛰기

모두 표준 오류, 경고가 하나라도 있으면 종료 코드 2(실패가 없을 때).

- `warning: Skipping file <path>. File does not exist.` (깨진 링크 포함)
- `warning: Skipping file <path>. File is character special device, block special device, FIFO, or socket.`
- `warning: Skipping file <path>. File/Directory is not readable.`
- `warning: File has an invalid timestamp. Passing epoch time as timestamp.` (건너뛰지 않음)
- `--no-follow-symlinks`면 링크를 조용히 건너뛴다.
- 글레이셔: `StorageClass`가 GLACIER 또는 DEEP_ARCHIVE이고 복원 완료 표시가 없으면 다운로드·복사를 건너뛴다. 목록 조회 결과에는 복원 정보가 없어 재귀·sync에서는 항상 건너뛴다. `--force-glacier-transfer`면 전송, `--ignore-glacier-warnings`면 조용히 건너뛴다.
  - 문구: `warning: Skipping file s3://<bucket/key>. Object is of storage class GLACIER. Unable to perform <op> operations on GLACIER objects. You must restore the object to be able to perform the operation. See aws s3 <op> help for additional parameter options to ignore or force these transfers.`
- 다운로드 대상이 상위 디렉터리를 가리키면 `warning: Skipping file <key>. File references a parent directory.`
- mtime 설정 실패: `warning: Skipping file <file>. Successfully Downloaded <file> but was unable to update the last modified time. <err>`

### A.8 출력 형식

- 출력기 선택 순서: `--quiet`(오류·경고 포함 아무것도 안 냄) → `--only-show-errors` 또는 스트림(진행률·성공 줄 없음, 실패·경고·dryrun 줄은 냄) → `--no-progress` → 기본.
- 성공: `{종류}: {원본} to {대상}`, 대상이 없으면 `{종류}: {원본}`. dryrun은 앞에 `(dryrun) `.
- 실패: `{종류} failed: {원본} to {대상} {오류}`. 치명적 오류: `fatal error: {오류}`.
- 종류는 upload, download, copy, delete, 그리고 mv에서는 move.
- 로컬 경로는 현재 디렉터리 기준 상대 경로로 표시한다(`./a.txt`, `sub/a.txt`, `../x/a.txt`). S3는 `s3://bucket/key`, 스트림은 `-`.
- 진행률(표준 출력, TTY 여부를 보지 않음):
  - `Completed {완료}/{전체} ({속도}/s) with {n} file(s) remaining`
  - 바이트가 없는 작업: `Completed {파일 수} file(s) with {n} file(s) remaining`
  - 전체 수를 모르는 동안 `{전체}`와 `{n}` 앞에 `~`, 끝에 ` (calculating...)`.
  - 이전 진행률 길이만큼 공백으로 채우고 `\r`로 끝난다. 결과 줄도 같은 길이로 채운 뒤 `\n`.
- 크기 표기: `1 Byte`, 1024 미만은 `N Bytes`, 그 위는 소수 한 자리 `KiB`…`EiB`. 1024 → `1.0 KiB`, 1048575 → `1.0 MiB`.
- `ls`:
  - 버킷: `YYYY-MM-DD HH:MM:SS 이름` (로컬 시간)
  - 접두어: 공백 27개 + `PRE` + 공백 + `이름/`
  - 객체: `날짜(19자) 크기(오른쪽 정렬 10자) 이름`
  - `--summarize`: `\nTotal Objects: N\n` + `   Total Size: S\n`
- `presign`: URL과 줄바꿈. `mb`: `make_bucket: <bucket>`. `rb`: `remove_bucket: <bucket>`. 실패는 `make_bucket failed: <path> <오류>` 형식으로 표준 오류, 종료 코드 1. `website`: 출력 없음.

### A.9 종료 코드

| 코드 | 조건 |
|---|---|
| 0 | 성공. 버킷 루트나 전체 버킷 `ls` 포함 |
| 1 | 전송 실패가 하나라도 있음. 목록 조회 중 오류(HeadObject 404, NoSuchBucket, AccessDenied). 키가 있는 `ls`에서 첫 페이지에 객체도 접두어도 없음. `mb`/`rb` API 실패 |
| 2 | 실패 없이 경고만 있음. argparse 인자 오류 |
| 255 | 그 밖의 예외: Unknown options, 잘못된 경로 조합, 없는 로컬 원본, 스트리밍 오용, SSE-C 검증, 같은 경로 `mv`, `ls`/`website`/`presign`의 API 오류, 리전·자격 증명·프로필 오류, 잘못된 `s3` 설정값 |

### A.10 환경변수와 설정

- 프로필: `AWS_DEFAULT_PROFILE`이 `AWS_PROFILE`보다 먼저, `--profile`이 둘보다 먼저.
- 리전: `AWS_DEFAULT_REGION`. 이 버전은 `AWS_REGION`을 읽지 않는다.
- `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CA_BUNDLE`, `AWS_MAX_ATTEMPTS`, `AWS_RETRY_MODE`.
- 엔드포인트: `AWS_ENDPOINT_URL_S3` → `AWS_ENDPOINT_URL` → `services` 섹션 → 프로필 `endpoint_url`. `AWS_IGNORE_CONFIGURED_ENDPOINT_URLS`로 끌 수 있다.
- `s3 =` 블록의 크기 값은 정수 또는 `KB/MB/GB/TB/KiB/MiB/GiB/TiB`(모두 1024 기반). `max_bandwidth`는 `B/s`로 끝나야 한다. 오류 문구: `Invalid size value: …`, `Value for %s must be a positive integer: %s`.

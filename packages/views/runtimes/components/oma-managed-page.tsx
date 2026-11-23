"use client";

import { useEffect, useId, useState } from "react";
import { AlertCircle, Plus } from "@multica/ui/icons";
import { toast } from "sonner";
import { useQuery } from "@tanstack/react-query";
import type { AgentRuntime } from "@multica/core/types";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { useWorkspacePresenceMap } from "@multica/core/agents";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import {
  MAX_CONCURRENCY,
  MAX_RUN_TIMEOUT,
  MIN_RUN_TIMEOUT,
} from "@multica/core/runtimes/qoder";
import {
  OMA_PROVIDER,
  isManagedOMARuntime,
  omaManagedOptions,
  useCheckOMA,
  useDisconnectOMA,
  useOMAEnvironments,
  useResumeOMA,
  useSaveOMA,
  useStopOMA,
  type OMAConnection,
  type OMAInput,
} from "@multica/core/runtimes/oma";
import {
  agentListOptions,
  memberListOptions,
} from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@multica/ui/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { cn } from "@multica/ui/lib/utils";
import { CollectionPageState } from "../../layout/collection-page";
import {
  OBJECT_PAGE_BODY,
  ObjectIdentity,
  ObjectPage,
  ObjectSection,
} from "../../layout/object-page";
import { AppLink, useNavigation } from "../../navigation";
import { createPathWithParams } from "../../agents/create/squad-param";
import { useT } from "../../i18n";
import { ProviderLogo } from "./provider-logo";
import { QoderSection } from "./qoder-connection-cards";
import { ServingAgentList } from "./runtime-detail";
import { parseActiveAgentsConflict } from "./delete-runtime-dialog";

const LAYOUT_ID = "multica_runtime_detail_layout";

/** Route locator of the workspace's OMA Managed Agents page. */
export const OMA_MANAGED_LOCATOR = "oma-managed";

/**
 * Whether a runtimes route locator names the managed OMA connection: its own
 * locator, the machine its runtime registers under, or that runtime's id.
 */
export function isOMAManagedLocator(
  locator: string,
  runtimes: AgentRuntime[],
): boolean {
  return (
    locator === OMA_MANAGED_LOCATOR ||
    locator.startsWith("cloud:oma-managed-") ||
    runtimes.some(
      (runtime) => runtime.id === locator && isManagedOMARuntime(runtime),
    )
  );
}

function isOMAURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    return (
      (parsed.protocol === "http:" || parsed.protocol === "https:") &&
      !!parsed.host &&
      !parsed.username &&
      !parsed.password &&
      !parsed.search &&
      !parsed.hash
    );
  } catch {
    return false;
  }
}

function sameOMAEndpoint(a: string, b: string): boolean {
  return a.trim().replace(/\/+$/, "") === b.trim().replace(/\/+$/, "");
}

export function OMAStatusBadge({
  connection,
}: {
  connection: OMAConnection;
}) {
  const { t } = useT("runtimes");
  if (!connection.configured) {
    return (
      <span className="text-caption text-muted-foreground">
        {t(($) => $.oma_managed.not_connected)}
      </span>
    );
  }
  const status = !connection.enabled ? "stopped" : connection.status;
  const tone =
    status === "online"
      ? "bg-success"
      : status === "error"
        ? "bg-destructive"
        : "bg-muted-foreground";
  const label =
    status === "online"
      ? t(($) => $.oma_managed.status.online)
      : status === "error"
        ? t(($) => $.oma_managed.status.error)
        : status === "stopped"
          ? t(($) => $.oma_managed.status.stopped)
          : status === "starting"
            ? t(($) => $.oma_managed.status.starting)
            : t(($) => $.oma_managed.status.offline);
  return (
    <span className="inline-flex items-center gap-1.5 text-caption text-muted-foreground">
      <span aria-hidden="true" className={cn("size-2 rounded-full", tone)} />
      {label}
    </span>
  );
}

type RepoRow = { url: string; token: string };

/** The saved connection as an input that keeps every stored secret. */
function savedOMAInput(data: OMAConnection): OMAInput {
  return {
    name: data.name,
    baseUrl: data.baseUrl,
    environmentId: data.environmentId,
    apiToken: "",
    vaultIds: data.vaultIds,
    maxConcurrency: data.maxConcurrency,
    runTimeoutMinutes: data.runTimeoutMinutes,
  };
}

/**
 * Connect or edit the workspace's OMA Managed Agents connection. Blank
 * secrets keep the saved ones, so editing never needs the key again.
 */
export function OMAConnectionForm({
  wsId,
  connection,
  onSaved,
}: {
  wsId: string;
  connection: OMAConnection;
  onSaved?: () => void;
}) {
  const { t } = useT("runtimes");
  const save = useSaveOMA(wsId);
  const check = useCheckOMA(wsId);
  const environments = useOMAEnvironments(wsId);
  const { mutate: loadEnvironments } = environments;
  const ids = {
    name: useId(),
    baseUrl: useId(),
    key: useId(),
    environment: useId(),
    vaults: useId(),
    concurrency: useId(),
    timeout: useId(),
  };
  const [name, setName] = useState(connection.name || "OMA Managed Agents");
  const [baseUrl, setBaseUrl] = useState(connection.baseUrl);
  const [token, setToken] = useState("");
  const [environmentId, setEnvironmentId] = useState(connection.environmentId);
  const [vaults, setVaults] = useState(connection.vaultIds.join(", "));
  const [repos, setRepos] = useState<RepoRow[]>(
    connection.repositories.map((url) => ({ url, token: "" })),
  );
  const [maxConcurrency, setMaxConcurrency] = useState(connection.maxConcurrency);
  const [runTimeoutMinutes, setRunTimeoutMinutes] = useState(
    connection.runTimeoutMinutes,
  );
  const trimmedBaseUrl = baseUrl.trim();
  const trimmedToken = token.trim();
  const baseUrlValid = isOMAURL(trimmedBaseUrl);
  const baseChanged =
    !sameOMAEndpoint(connection.baseUrl, trimmedBaseUrl) &&
    (!!connection.baseUrl ||
      connection.hasToken ||
      connection.repositories.length > 0);
  const canListEnvironments =
    baseUrlValid &&
    (!!trimmedToken || (!baseChanged && connection.hasToken));

  // A new key lists its own environments; a saved key lists the saved ones.
  useEffect(() => {
    if (!canListEnvironments) return;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      loadEnvironments(
        { baseUrl: trimmedBaseUrl, token: trimmedToken, signal: controller.signal },
        {
          onSuccess: (items) => {
            if (controller.signal.aborted) return;
            setEnvironmentId((current) =>
              items.some((item) => item.id === current)
                ? current
                : items.length === 1
                  ? items[0]!.id
                  : "",
            );
          },
        },
      );
    }, 600);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [trimmedBaseUrl, trimmedToken, canListEnvironments, loadEnvironments]);

  const known = new Set(baseChanged ? [] : connection.repositories);
  const reposValid = repos.every(
    (row) =>
      row.url.trim().length > 0 &&
      (row.token.trim().length > 0 || known.has(row.url.trim())),
  );
  const limitsValid =
    maxConcurrency >= 1 &&
    maxConcurrency <= MAX_CONCURRENCY &&
    runTimeoutMinutes >= MIN_RUN_TIMEOUT &&
    runTimeoutMinutes <= MAX_RUN_TIMEOUT;
  const input = (): OMAInput => ({
    name: name.trim(),
    baseUrl: trimmedBaseUrl,
    environmentId,
    apiToken: trimmedToken,
    githubTokens: Object.fromEntries(
      repos.map((row) => [row.url.trim(), row.token.trim()]),
    ),
    vaultIds: vaults
      .split(",")
      .map((id) => id.trim())
      .filter(Boolean),
    maxConcurrency,
    runTimeoutMinutes,
  });
  const ready =
    !!name.trim() &&
    baseUrlValid &&
    !!environmentId &&
    canListEnvironments &&
    reposValid &&
    limitsValid;
  const busy = save.isPending || check.isPending;

  return (
    <form
      className="space-y-5"
      onSubmit={(event) => {
        event.preventDefault();
        if (!ready || busy) return;
        save.mutate(input(), {
          onSuccess: () => {
            setToken("");
            setRepos((current) =>
              current.map((row) => ({ ...row, token: "" })),
            );
            toast.success(t(($) => $.oma_managed.saved));
            onSaved?.();
          },
        });
      }}
    >
      <div className="space-y-2">
        <label htmlFor={ids.name} className="text-caption font-medium">
          {t(($) => $.oma_managed.name)}
        </label>
        <Input
          id={ids.name}
          value={name}
          disabled={busy}
          onChange={(event) => setName(event.target.value)}
        />
      </div>
      <div className="space-y-2">
        <label htmlFor={ids.baseUrl} className="text-caption font-medium">
          {t(($) => $.oma_managed.base_url)}
        </label>
        <Input
          id={ids.baseUrl}
          type="url"
          autoComplete="off"
          placeholder="http://oma.internal:38080/v1"
          value={baseUrl}
          disabled={busy}
          onChange={(event) => setBaseUrl(event.target.value)}
        />
        <p className="text-caption text-muted-foreground">
          {t(($) => $.oma_managed.base_url_hint)}
        </p>
      </div>
      <div className="space-y-2">
        <label htmlFor={ids.key} className="text-caption font-medium">
          {t(($) => $.oma_managed.api_key)}
        </label>
        <Input
          id={ids.key}
          type="password"
          autoComplete="off"
          placeholder={
            connection.hasToken
              ? t(($) => $.oma_managed.keep_secret)
              : t(($) => $.oma_managed.api_key_placeholder)
          }
          value={token}
          disabled={busy}
          onChange={(event) => setToken(event.target.value)}
        />
        <p className="text-caption text-muted-foreground">
          {t(($) => $.oma_managed.api_key_hint)}
        </p>
      </div>
      <div className="space-y-2">
        <p id={ids.environment} className="text-caption font-medium">
          {t(($) => $.oma_managed.environment)}
        </p>
        <Select
          items={(environments.data ?? []).map((item) => ({
            value: item.id,
            label: item.name,
          }))}
          value={environmentId || null}
          onValueChange={(next) => setEnvironmentId(next ?? "")}
          disabled={busy || !environments.data?.length}
        >
          <SelectTrigger className="w-full" aria-labelledby={ids.environment}>
            <SelectValue
              placeholder={t(($) => $.oma_managed.select_environment)}
            />
          </SelectTrigger>
          <SelectContent alignItemWithTrigger={false}>
            {(environments.data ?? []).map((item) => (
              <SelectItem key={item.id} value={item.id}>
                {item.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {environments.isPending && (
          <p role="status" className="text-caption text-muted-foreground">
            {t(($) => $.oma_managed.loading)}
          </p>
        )}
        {environments.isError && (
          <p role="alert" className="text-caption text-destructive">
            {environments.error.message}
          </p>
        )}
        {environments.isSuccess && !environments.data.length && (
          <p role="status" className="text-caption text-muted-foreground">
            {t(($) => $.oma_managed.no_environments)}
          </p>
        )}
      </div>
      <div className="space-y-2">
        <label htmlFor={ids.vaults} className="text-caption font-medium">
          {t(($) => $.oma_managed.vaults)}
        </label>
        <Input
          id={ids.vaults}
          // eslint-disable-next-line no-restricted-syntax -- the vault ID prefix, a technical value
          placeholder="vlt_…"
          autoComplete="off"
          spellCheck={false}
          value={vaults}
          disabled={busy}
          onChange={(event) => setVaults(event.target.value)}
        />
        <p className="text-caption text-muted-foreground">
          {t(($) => $.oma_managed.vaults_hint)}
        </p>
      </div>
      <fieldset className="space-y-2">
        <legend className="text-caption font-medium">
          {t(($) => $.oma_managed.repositories)}
        </legend>
        <p className="text-caption text-muted-foreground">
          {t(($) => $.oma_managed.repositories_hint)}
        </p>
        {repos.map((row, index) => (
          <div key={index} className="grid gap-2 sm:grid-cols-[1fr_1fr_auto]">
            <Input
              type="url"
              aria-label={t(($) => $.oma_managed.github_url)}
              placeholder="https://github.com/owner/repo"
              value={row.url}
              disabled={busy}
              onChange={(event) =>
                setRepos(
                  repos.map((item, i) =>
                    i === index ? { ...item, url: event.target.value } : item,
                  ),
                )
              }
            />
            <Input
              type="password"
              aria-label={t(($) => $.oma_managed.github_token)}
              autoComplete="off"
              placeholder={
                known.has(row.url.trim())
                  ? t(($) => $.oma_managed.keep_secret)
                  : t(($) => $.oma_managed.github_token)
              }
              value={row.token}
              disabled={busy}
              onChange={(event) =>
                setRepos(
                  repos.map((item, i) =>
                    i === index ? { ...item, token: event.target.value } : item,
                  ),
                )
              }
            />
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={() => setRepos(repos.filter((_, i) => i !== index))}
            >
              {t(($) => $.oma_managed.remove)}
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={busy}
          onClick={() => setRepos([...repos, { url: "", token: "" }])}
        >
          {t(($) => $.oma_managed.add_repository)}
        </Button>
      </fieldset>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-2">
          <label htmlFor={ids.concurrency} className="text-caption font-medium">
            {t(($) => $.oma_managed.concurrency)}
          </label>
          <Input
            id={ids.concurrency}
            type="number"
            min={1}
            max={MAX_CONCURRENCY}
            value={maxConcurrency}
            disabled={busy}
            onChange={(event) => setMaxConcurrency(Number(event.target.value))}
          />
        </div>
        <div className="space-y-2">
          <label htmlFor={ids.timeout} className="text-caption font-medium">
            {t(($) => $.oma_managed.run_timeout)}
          </label>
          <Input
            id={ids.timeout}
            type="number"
            min={MIN_RUN_TIMEOUT}
            max={MAX_RUN_TIMEOUT}
            value={runTimeoutMinutes}
            disabled={busy}
            onChange={(event) =>
              setRunTimeoutMinutes(Number(event.target.value))
            }
          />
        </div>
      </div>
      {(save.isError || check.isError) && (
        <p role="alert" className="text-caption text-destructive">
          {(save.error ?? check.error)?.message}
        </p>
      )}
      {check.isSuccess && (
        <p role="status" className="text-caption text-muted-foreground">
          {t(($) => $.oma_managed.check_passed)}
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={!ready || busy}>
          {save.isPending
            ? t(($) => $.oma_managed.working)
            : connection.configured
              ? t(($) => $.oma_managed.save)
              : t(($) => $.oma_managed.connect)}
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={!ready || busy}
          onClick={() => check.mutate(input())}
        >
          {check.isPending
            ? t(($) => $.oma_managed.working)
            : t(($) => $.oma_managed.check)}
        </Button>
      </div>
    </form>
  );
}

/** First-time connection; an existing one is managed on its runtime page. */
export function OMAConnectDialog({
  onClose,
  onConnected,
}: {
  onClose: () => void;
  onConnected?: () => void;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const connection = useQuery(omaManagedOptions(wsId));
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="max-h-[85%] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t(($) => $.oma_managed.connect_title)}</DialogTitle>
          <DialogDescription>
            {t(($) => $.oma_managed.cloud_description)}
          </DialogDescription>
        </DialogHeader>
        {connection.data?.available === false ? (
          <p role="alert" className="text-body text-destructive">
            {t(($) => $.oma_managed.unavailable)}
          </p>
        ) : connection.isError ? (
          <p role="alert" className="text-body text-destructive">
            {connection.error.message}
          </p>
        ) : connection.data ? (
          <OMAConnectionForm
            wsId={wsId}
            connection={connection.data}
            onSaved={() => {
              onConnected?.();
              onClose();
            }}
          />
        ) : (
          <p role="status" className="text-caption text-muted-foreground">
            {t(($) => $.oma_managed.loading)}
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}

/** The workspace's OMA Managed Agents connection and its managed runtime. */
export function OMAManagedPage() {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const userId = useAuthStore((state) => state.user?.id ?? null);
  const connection = useQuery(omaManagedOptions(wsId));
  const { data: runtimes = [], refetch: refetchRuntimes } = useQuery(
    runtimeListOptions(wsId),
  );
  const { data: agents = [], refetch: refetchAgents } = useQuery(
    agentListOptions(wsId),
  );
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { byAgent: presenceMap } = useWorkspacePresenceMap(wsId);
  const stop = useStopOMA(wsId);
  const resume = useResumeOMA(wsId);
  const disconnect = useDisconnectOMA(wsId);
  const [disconnectOpen, setDisconnectOpen] = useState(false);

  const title = t(($) => $.oma_managed.cloud_title);
  const segments = [{ href: paths.runtimes(), label: t(($) => $.page.title) }];
  const leaf = (
    <span className="flex min-w-0 items-center gap-1.5">
      <ProviderLogo provider={OMA_PROVIDER} className="size-3.5 shrink-0" />
      <span className="truncate">{title}</span>
    </span>
  );

  const data = connection.data;
  if (connection.isPending) {
    return (
      <ObjectPage type="runtime" layoutId={LAYOUT_ID} segments={segments} leaf={leaf}>
        <OMABodySkeleton />
      </ObjectPage>
    );
  }
  if (connection.isError || data?.available !== true) {
    return (
      <ObjectPage type="runtime" layoutId={LAYOUT_ID} segments={segments} leaf={leaf}>
        <CollectionPageState
          icon={AlertCircle}
          tone="destructive"
          title={
            connection.isError
              ? connection.error.message
              : t(($) => $.oma_managed.unavailable)
          }
          actions={
            <Button
              size="sm"
              variant="outline"
              render={<AppLink href={paths.runtimes()} />}
              nativeButton={false}
            >
              {t(($) => $.detail.all_runtimes)}
            </Button>
          }
        />
      </ObjectPage>
    );
  }

  const member = members.find((m) => m.user_id === userId);
  const isAdmin = member?.role === "owner" || member?.role === "admin";
  const runtime = runtimes.find((item) => isManagedOMARuntime(item)) ?? null;
  const boundAgents = agents.filter(
    (agent) => !agent.archived_at && agent.runtime_id === runtime?.id,
  );
  const ready =
    data.configured && data.enabled && data.status === "online" && !!runtime;
  const busy = stop.isPending || resume.isPending || disconnect.isPending;
  const failing = data.enabled && data.status === "error" && !!data.lastError;

  return (
    <ObjectPage
      type="runtime"
      layoutId={LAYOUT_ID}
      segments={segments}
      leaf={leaf}
      actions={
        ready && runtime ? (
          <Button
            size="sm"
            onClick={() =>
              navigation.push(
                createPathWithParams(paths.newAgentManual(), {
                  runtime: runtime.id,
                }),
              )
            }
          >
            <Plus aria-hidden="true" className="size-3.5" />
            {t(($) => $.oma_managed.create_agent)}
          </Button>
        ) : undefined
      }
    >
      <div className={cn(OBJECT_PAGE_BODY, "space-y-8")}>
        <div className="space-y-3">
          <ObjectIdentity
            media={
              <div className="flex size-12 items-center justify-center rounded-xl border bg-muted/50">
                <ProviderLogo provider={OMA_PROVIDER} className="size-6" />
              </div>
            }
            title={title}
            trailing={<OMAStatusBadge connection={data} />}
            description={
              data.configured ? (
                <span className="tabular-nums">
                  {t(($) => $.oma_managed.agent_count, {
                    count: boundAgents.length,
                  })}
                  {" · "}
                  {t(($) => $.oma_managed.usage, {
                    active: data.activeRuns,
                    limit: data.maxConcurrency,
                    queued: data.queuedRuns,
                  })}
                </span>
              ) : undefined
            }
          />
          {failing && (
            <p
              role="alert"
              className="rounded-lg border border-destructive/20 bg-destructive/5 px-3 py-2 text-caption text-destructive"
            >
              {data.lastError}
            </p>
          )}
        </div>

        <QoderSection title={t(($) => $.oma_managed.connection)}>
          <div className="p-4">
            {isAdmin ? (
              <OMAConnectionForm
                key={data.configured ? "edit" : "connect"}
                wsId={wsId}
                connection={data}
              />
            ) : (
              <p className="text-caption text-muted-foreground">
                {data.configured
                  ? data.baseUrl
                  : t(($) => $.oma_managed.not_connected)}
              </p>
            )}
          </div>
        </QoderSection>

        <ObjectSection
          title={t(($) => $.page.columns.agents)}
          count={boundAgents.length}
        >
          <ServingAgentList
            agents={boundAgents}
            presenceMap={presenceMap}
            agentHref={(id) => paths.agentDetail(id)}
          />
        </ObjectSection>

        {isAdmin && data.configured && (
          <QoderSection title={t(($) => $.oma_managed.manage)}>
            <div className="flex flex-wrap gap-2 p-4">
              {data.enabled ? (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() =>
                    stop.mutate(undefined, {
                      onError: (err) => toast.error(err.message),
                    })
                  }
                >
                  {t(($) => $.oma_managed.pause)}
                </Button>
              ) : (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy}
                  onClick={() =>
                    resume.mutate(savedOMAInput(data), {
                      onError: (err) => toast.error(err.message),
                    })
                  }
                >
                  {t(($) => $.oma_managed.resume)}
                </Button>
              )}
              <Button
                variant="destructive"
                size="sm"
                disabled={busy}
                onClick={() => setDisconnectOpen(true)}
              >
                {t(($) => $.oma_managed.disconnect)}
              </Button>
            </div>
          </QoderSection>
        )}
      </div>
      <AlertDialog
        open={disconnectOpen}
        onOpenChange={(open) => {
          if (!open && !disconnect.isPending) setDisconnectOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.oma_managed.disconnect_confirm)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.oma_managed.disconnect_hint)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {boundAgents.length > 0 && (
            <ul className="list-disc space-y-0.5 pl-5 text-caption">
              {boundAgents.map((agent) => (
                <li key={agent.id}>{agent.name}</li>
              ))}
            </ul>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnect.isPending}>
              {t(($) => $.qoder.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={disconnect.isPending}
              onClick={() =>
                disconnect.mutate(
                  boundAgents.map((agent) => agent.id),
                  {
                    onSuccess: () => {
                      setDisconnectOpen(false);
                      toast.success(t(($) => $.oma_managed.disconnected));
                      navigation.replace(paths.runtimes());
                    },
                    onError: (err) => {
                      setDisconnectOpen(false);
                      if (parseActiveAgentsConflict(err)) {
                        void connection.refetch();
                        void refetchAgents();
                        void refetchRuntimes();
                      }
                      toast.error(err.message);
                    },
                  },
                )
              }
            >
              {t(($) => $.oma_managed.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </ObjectPage>
  );
}

function OMABodySkeleton() {
  return (
    <div className={cn(OBJECT_PAGE_BODY, "space-y-8")}>
      <div className="flex items-center gap-3">
        <Skeleton className="size-12 rounded-xl" />
        <div className="flex-1 space-y-2">
          <Skeleton className="h-5 w-56" />
          <Skeleton className="h-4 w-40" />
        </div>
      </div>
      <Skeleton className="h-64 w-full rounded-lg" />
    </div>
  );
}

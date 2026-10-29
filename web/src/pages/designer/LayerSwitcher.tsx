import { Avatar, Badge } from "@braedonsaunders/appkit-ui";
import { Globe2 } from "lucide-react";
import { useMemo } from "react";
import { viewerName } from "../../components/pickers";
import { ToolbarPicker, type ToolbarOption } from "../../components/ToolbarPicker";
import type { HomeLayout, Viewer } from "../../lib/contract";
import { t, tn } from "../../i18n";
import { overrideCount, type LayerKey } from "../../lib/layout";
import { useLocale } from "../../i18n/react";

const globe = (size: number) => (
  <span
    className="grid shrink-0 place-items-center rounded-full bg-primary-subtle text-primary"
    style={{ width: size, height: size }}
  >
    <Globe2 size={Math.round(size * 0.55)} />
  </span>
);

/**
 * Which layer the controls edit: the Plex Home for everyone, or one viewer's overrides.
 * A searchable dropdown, so it stays one line however many viewers there are;
 * viewers who already override something are listed first.
 */
export function LayerSwitcher({
  layer,
  onChange,
  viewers,
  draft,
}: {
  layer: LayerKey;
  onChange: (l: LayerKey) => void;
  viewers: Viewer[];
  draft: HomeLayout;
}) {
  const { locale } = useLocale();
  const options = useMemo(() => {
    const counted = viewers.map((v) => ({ v, n: overrideCount(draft, v.id) }));
    counted.sort(
      (a, b) => Number(b.n > 0) - Number(a.n > 0) || viewerName(a.v).localeCompare(viewerName(b.v)),
    );
    const out: ToolbarOption<string>[] = [
      {
        value: "global",
        label: t("Everyone"),
        hint: tn(
          viewers.length,
          "The Plex Home all {count} viewer get",
          "The Plex Home all {count} viewers get",
        ),
        icon: globe(26),
      },
      ...counted.map(({ v, n }) => ({
        value: String(v.id),
        label: viewerName(v),
        hint: v.isOwner ? t("Owner") : `@${v.username}`,
        icon: <Avatar src={v.thumb} name={viewerName(v)} size={26} />,
        group: t("Overrides for one viewer"),
        meta: n ? (
          <Badge variant="info" className="shrink-0">
            {tn(n, "{count} override", "{count} overrides")}
          </Badge>
        ) : undefined,
      })),
    ];
    return out;
  }, [viewers, draft, locale]);

  const current = layer === "global" ? null : viewers.find((v) => v.id === layer);
  const count = layer === "global" ? 0 : overrideCount(draft, layer);
  return (
    <ToolbarPicker
      ariaLabel={t("Editing layer")}
      value={String(layer)}
      onChange={(v) => onChange(v === "global" ? "global" : Number(v))}
      options={options}
      searchable={viewers.length > 6}
      searchPlaceholder={t("Find a viewer…")}
      className="min-w-[8.5rem] flex-1 justify-start"
      trigger={
        <>
          {current ? (
            <Avatar src={current.thumb} name={viewerName(current)} size={20} />
          ) : (
            globe(20)
          )}
          <span className="min-w-0 flex-1 truncate text-left">
            {current
              ? viewerName(current)
              : layer === "global"
                ? t("Everyone")
                : t("Viewer {id}", { id: layer })}
          </span>
          {count ? (
            <span className="shrink-0 rounded-full bg-info/15 px-1.5 text-[11px] font-semibold tabular-nums text-info">
              {count}
            </span>
          ) : null}
        </>
      }
    />
  );
}

import { Drawer } from "../../../components/Drawer";
import { t } from "../../../i18n";
import type { AssetKind, LibraryAsset } from "../onair/studio-model";
import { LibraryGrid } from "./Library";
export function LibraryPicker(p: {
  open: boolean;
  kinds: AssetKind[];
  onPick(asset: LibraryAsset): void;
  onClose(): void;
}) {
  return (
    <Drawer open={p.open} onClose={p.onClose} title={t("Choose from Library")} size="xl">
      {p.open && (
        <LibraryGrid
          allowed={p.kinds}
          onPick={(a) => {
            p.onPick(a);
            p.onClose();
          }}
        />
      )}
    </Drawer>
  );
}

/* Resolve vaMapBuffer2 the same way the bundled ffmpeg does: dlopen
   libva.so.2 and dlsym. Exit 0 when the loaded libva defines it, 1 when
   dlsym reports it missing, 2 when libva itself cannot be loaded. */
#include <dlfcn.h>
#include <stdio.h>

int main(void) {
	void *handle = dlopen("libva.so.2", RTLD_NOW);
	if (!handle) {
		fprintf(stderr, "dlopen: %s\n", dlerror());
		return 2;
	}
	(void)dlerror();
	void *sym = dlsym(handle, "vaMapBuffer2");
	const char *err = dlerror();
	if (err) {
		fprintf(stderr, "dlsym: %s\n", err);
		return 1;
	}
	printf("vaMapBuffer2 %p\n", sym);
	return 0;
}

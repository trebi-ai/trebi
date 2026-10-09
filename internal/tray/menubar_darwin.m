//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#include "menubar_darwin.h"
#include "_cgo_export.h"

static uintptr_t handle;

@interface TrayTarget : NSObject
- (void)clicked:(NSMenuItem *)item;
@end

@implementation TrayTarget
- (void)clicked:(NSMenuItem *)item {
	trayClicked(handle, (int)item.tag);
}
@end

static NSStatusItem *statusItem;
static TrayTarget *target;

void trayRun(uintptr_t h) {
	handle = h;
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		target = [TrayTarget new];
		statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
		statusItem.menu = [NSMenu new];
		statusItem.menu.autoenablesItems = NO;
		trayReady(handle);
		[NSApp run];
	}
}

// The favicon colors (oklch 0.526 0.199 268, dark 0.67 0.174 270.6).
static NSColor *brandColor(void) {
	NSAppearanceName name = [NSAppearance.currentDrawingAppearance
		bestMatchFromAppearancesWithNames:@[ NSAppearanceNameAqua, NSAppearanceNameDarkAqua ]];
	if ([name isEqualToString:NSAppearanceNameDarkAqua]) {
		return [NSColor colorWithSRGBRed:0.431 green:0.545 blue:1.0 alpha:1];
	}
	return [NSColor colorWithSRGBRed:0.231 green:0.357 blue:0.86 alpha:1];
}

// png is a shape mask. The handler paints it at draw time, so the color
// follows the menu bar appearance.
void traySetIcon(const void *png, int len, const char *title) {
	NSData *data = [NSData dataWithBytes:png length:len];
	NSString *text = [NSString stringWithUTF8String:title];
	dispatch_async(dispatch_get_main_queue(), ^{
		NSImage *mask = [[NSImage alloc] initWithData:data];
		NSImage *image = [NSImage imageWithSize:NSMakeSize(18, 18) flipped:NO drawingHandler:^BOOL(NSRect rect) {
			[mask drawInRect:rect];
			[brandColor() set];
			NSRectFillUsingOperation(rect, NSCompositingOperationSourceIn);
			return YES;
		}];
		statusItem.button.image = image;
		statusItem.button.imagePosition = text.length ? NSImageLeft : NSImageOnly;
		statusItem.button.title = text;
	});
}

// json: [{"text": "...", "id": 1}, {"separator": true}, ...]. id 0 is disabled.
void traySetMenu(const char *json) {
	NSData *data = [NSData dataWithBytes:json length:strlen(json)];
	dispatch_async(dispatch_get_main_queue(), ^{
		NSArray *items = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
		NSMenu *menu = statusItem.menu;
		[menu removeAllItems];
		for (NSDictionary *it in items) {
			if ([it[@"separator"] boolValue]) {
				[menu addItem:[NSMenuItem separatorItem]];
				continue;
			}
			NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:it[@"text"] action:nil keyEquivalent:@""];
			NSInteger tag = [it[@"id"] integerValue];
			if (tag > 0) {
				item.tag = tag;
				item.target = target;
				item.action = @selector(clicked:);
			}
			item.enabled = tag > 0;
			[menu addItem:item];
		}
	});
}

// Blocks until the user answers. Returns 1 for ok, 0 for cancel. Never call
// it on the main thread.
int trayAlert(const char *message, const char *info, const char *ok, const char *cancel) {
	NSString *m = [NSString stringWithUTF8String:message];
	NSString *i = [NSString stringWithUTF8String:info];
	NSString *o = [NSString stringWithUTF8String:ok];
	NSString *c = [NSString stringWithUTF8String:cancel];
	__block int result = 0;
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSAlert *alert = [NSAlert new];
		alert.messageText = m;
		alert.informativeText = i;
		[alert addButtonWithTitle:o];
		if (c.length) {
			[alert addButtonWithTitle:c];
		}
		[NSApp activateIgnoringOtherApps:YES];
		result = [alert runModal] == NSAlertFirstButtonReturn ? 1 : 0;
	});
	return result;
}

void trayTerminate(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp terminate:nil];
	});
}

# Base image configuration. This allows switching to UBI-minimal for
# enterprise images while keeping distroless as the default.

def select_base_image_for_go_services():
    """Select base image for Go services (init-container, cockroach-operator).
    
    Returns:
        A select() statement that chooses between distroless (default) and UBI-minimal
    """
    return select({
        "//build:use_ubi": "@ubi-minimal",
        "//conditions:default": "@distroless",
    })

def select_base_image_for_inotifywait():
    """Select base image for inotifywait service.
    
    Returns:
        A select() statement that chooses between distroless-based and UBI-based inotifywait-base
    """
    return select({
        "//build:use_ubi": "@inotifywait-base-ubi",
        "//conditions:default": "@inotifywait-base-distroless",
    })



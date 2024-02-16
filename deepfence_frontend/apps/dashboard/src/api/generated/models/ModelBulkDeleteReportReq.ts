/* tslint:disable */
/* eslint-disable */
/**
 * Deepfence ThreatMapper
 * Deepfence Runtime API provides programmatic control over Deepfence microservice securing your container, kubernetes and cloud deployments. The API abstracts away underlying infrastructure details like cloud provider,  container distros, container orchestrator and type of deployment. This is one uniform API to manage and control security alerts, policies and response to alerts for microservices running anywhere i.e. managed pure greenfield container deployments or a mix of containers, VMs and serverless paradigms like AWS Fargate.
 *
 * The version of the OpenAPI document: 2.0.0
 * Contact: community@deepfence.io
 *
 * NOTE: This class is auto generated by OpenAPI Generator (https://openapi-generator.tech).
 * https://openapi-generator.tech
 * Do not edit the class manually.
 */

import { exists, mapValues } from '../runtime';
/**
 * 
 * @export
 * @interface ModelBulkDeleteReportReq
 */
export interface ModelBulkDeleteReportReq {
    /**
     * 
     * @type {Array<string>}
     * @memberof ModelBulkDeleteReportReq
     */
    report_ids: Array<string> | null;
}

/**
 * Check if a given object implements the ModelBulkDeleteReportReq interface.
 */
export function instanceOfModelBulkDeleteReportReq(value: object): boolean {
    let isInstance = true;
    isInstance = isInstance && "report_ids" in value;

    return isInstance;
}

export function ModelBulkDeleteReportReqFromJSON(json: any): ModelBulkDeleteReportReq {
    return ModelBulkDeleteReportReqFromJSONTyped(json, false);
}

export function ModelBulkDeleteReportReqFromJSONTyped(json: any, ignoreDiscriminator: boolean): ModelBulkDeleteReportReq {
    if ((json === undefined) || (json === null)) {
        return json;
    }
    return {
        
        'report_ids': json['report_ids'],
    };
}

export function ModelBulkDeleteReportReqToJSON(value?: ModelBulkDeleteReportReq | null): any {
    if (value === undefined) {
        return undefined;
    }
    if (value === null) {
        return null;
    }
    return {
        
        'report_ids': value.report_ids,
    };
}
